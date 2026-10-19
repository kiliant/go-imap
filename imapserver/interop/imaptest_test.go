//go:build interop

package interop

// Dovecot's imaptest driven against imapserver+memory. See client_test.go for
// why third-party software is a separate shape from the interop matrix.
//
// SERVER-DESIGN.md §7 ranks this the highest-value external check available.
// The reason is specific rather than reputational: imaptest encodes what real
// clients actually do and what real servers actually got wrong, so it probes
// sequencing our own tests do not think to probe. A test suite written
// alongside a server inherits that server's reading of the RFC; this one does
// not.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const imaptestImage = "go-imap-imaptest:local"

// imaptestArgs is the connection half of every invocation below.
func imaptestArgs(host, port string) string {
	return fmt.Sprintf("host=%s port=%s user=%s pass=%s",
		host, port, interopUser, interopPassword)
}

// mboxFixture is the message source imaptest's stress mode appends from. It
// wants a real mbox file and ships none; upstream points at a sample hosted on
// dovecot.org, which would make this test depend on a third party's web server
// staying up. A generated one exercises the same paths.
//
// CRLF line endings inside the messages are deliberate: that is what imaptest's
// own sample uses, and it is what a message arrives as over IMAP.
func mboxFixture(count int) string {
	var builder strings.Builder
	for i := range count {
		fmt.Fprintf(&builder,
			"From sender@example.test Mon Aug 10 12:00:00 2026\n"+
				"From: sender@example.test\r\n"+
				"To: recipient@example.test\r\n"+
				"Subject: imaptest fixture %d\r\n"+
				"Message-ID: <fixture-%d@example.test>\r\n"+
				"Date: Mon, 10 Aug 2026 12:00:00 +0000\r\n"+
				"\r\n"+
				"Body of fixture message %d.\r\n"+
				"Second line, so the body is not degenerate.\r\n"+
				"\n", i, i, i)
	}
	return builder.String()
}

// TestImaptestScriptedConformance runs imaptest's own scripted test corpus.
//
// Each script is a transcript: commands to send and the responses a conforming
// server must produce, including the untagged responses and response codes that
// are easy to omit without any client noticing until one does. This is the
// closest thing to an executable conformance suite the IMAP ecosystem has.
//
// It currently does not run against us, and the reason is a limitation of the
// tool rather than a finding about the server — see the triaged exception in
// docs/INTEROP.md. imaptest's script runner refuses to start unless the server
// advertises LITERAL+, aborting with "FIXME: Add support for sync literals":
// it cannot drive a synchronising literal at all. imapserver advertises
// LITERAL- (RFC 7888), which caps unsolicited non-synchronising literals at
// 4096 octets, so the runner bails before sending a command.
//
// The skip is loud and specific on purpose. The task spec allows documented,
// triaged exceptions and forbids silent skips, and an earlier version of this
// test reported PASS on that very abort, because a tool refusing to start looks
// identical to a tool finding nothing wrong.
func TestImaptestScriptedConformance(t *testing.T) {
	for _, backend := range clientBackends {
		t.Run(backend.name, func(t *testing.T) { testImaptestScriptedConformance(t, backend.factory) })
	}
}

func testImaptestScriptedConformance(t *testing.T, factory backendFactory) {
	runtime := runtimeForTests(t)
	buildImage(t, runtime, imaptestImage, "testdata/imaptest")
	port := serverForClients(t, factory)

	// imaptest wants an empty account: the scripts append their own messages
	// and assert on the resulting sequence numbers and UIDs.
	script := fmt.Sprintf(`set -e
tests=""
for candidate in /usr/local/share/imaptest/tests /usr/share/imaptest/tests /src/imaptest/src/tests /src/imaptest/tests; do
  if [ -d "$candidate" ]; then tests="$candidate"; break; fi
done
if [ -z "$tests" ]; then echo "NOTESTS"; exit 0; fi
echo "TESTDIR=$tests"
imaptest %s test="$tests" || true
echo "=== done ==="
`, imaptestArgs(runtime.hostAlias, port))

	out, err := runInImage(t, runtime, imaptestImage, script, 15*time.Minute)
	t.Logf("imaptest scripted output:\n%s", out)
	if err != nil {
		t.Fatalf("running imaptest: %v", err)
	}
	if strings.Contains(out, "NOTESTS") {
		t.Skip("this imaptest build installed no scripted test corpus")
	}
	if strings.Contains(out, "Add support for sync literals") {
		t.Skip("TRIAGED: imaptest's script runner requires LITERAL+ and cannot " +
			"drive a synchronising literal; imapserver advertises LITERAL-. " +
			"A tool limitation, not a server finding — see docs/INTEROP.md")
	}
	requireImaptestRan(t, out)
	assertNoImaptestFailures(t, out)
	if len(imaptestSummaries(out)) == 0 {
		t.Error("imaptest scripts produced no recognised result summary")
	}
}

// TestImaptestStress runs imaptest's randomised command mix for a bounded time.
//
// This is the half the scripted corpus cannot reach, and — given the scripted
// runner skips above — currently the whole of what imaptest contributes:
// concurrent clients issuing overlapping commands against one mailbox, which is
// where selection teardown, update delivery and expunge sequencing race. The
// bar is the fuzz targets' — no crash, no hang, no protocol complaint — rather
// than a pass count.
func TestImaptestStress(t *testing.T) {
	for _, backend := range clientBackends {
		t.Run(backend.name, func(t *testing.T) { testImaptestStress(t, backend.factory) })
	}
}

func testImaptestStress(t *testing.T, factory backendFactory) {
	runtime := runtimeForTests(t)
	buildImage(t, runtime, imaptestImage, "testdata/imaptest")
	port := serverForClients(t, factory)

	// Recorded, so imaptest's command mix seeds the fuzz corpus too. It reaches
	// command shapes no client in this repository generates, which is the whole
	// argument for capturing rather than writing seeds.
	recorded := newRecorder(t, port)
	defer recorded.writeCorpus(t, "imaptest")

	script := fmt.Sprintf(`set -e
cat > /tmp/mbox <<'MBOXEOF'
%s
MBOXEOF
imaptest %s mbox=/tmp/mbox clients=4 secs=20 || true
echo "=== done ==="
`, mboxFixture(8), imaptestArgs(runtime.hostAlias, recorded.port()))

	out, err := runInImage(t, runtime, imaptestImage, script, 10*time.Minute)
	t.Logf("imaptest stress output:\n%s", out)
	if err != nil {
		t.Fatalf("running imaptest: %v", err)
	}
	if !strings.Contains(out, "=== done ===") {
		t.Fatal("imaptest did not finish; the server most likely stopped answering")
	}
	requireImaptestRan(t, out)
	assertNoImaptestFailures(t, out)
}

// requireImaptestRan fails when imaptest aborted before exercising the server.
//
// This exists because the failure mode it catches is silent: imaptest reports a
// startup problem on stdout, exits, and produces no complaints — which every
// "look for error lines" assertion reads as a clean run. A green test that
// never contacted the server is worse than a red one.
func requireImaptestRan(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Fatal:") {
			t.Fatalf("imaptest aborted before testing anything: %q", strings.TrimSpace(line))
		}
	}
}

// imaptestError matches the lines imaptest uses to report a server that broke
// the protocol. It reports these on stdout and still exits zero, so the exit
// status alone proves nothing.
var imaptestError = regexp.MustCompile(`(?i)\b(error|failed|mismatch|invalid|unexpected|bug|fatal)\b`)

// triaged is reserved for complaints imaptest makes that are known, recorded
// findings rather than regressions. It is intentionally empty: all server
// findings currently reported by the stress workload are fixed.
//
// They are listed rather than filtered away wholesale because the point of this
// test is to catch the *next* finding. A blanket "ignore errors" would make the
// run permanently green and permanently useless; an unconditional failure would
// make it permanently red, which CLAUDE.md says is a matrix nobody reads.
// Deleting an entry here when the underlying bug is fixed is the intended
// lifecycle — and if the bug regresses, the entry stops matching nothing and
// starts matching again, which no assertion would have caught either way, so
// each has a loopback regression test alongside the fix when it lands.
var triaged = []struct {
	pattern *regexp.Regexp
	finding string
}{
	// The pipelined-EXPUNGE entry that used to head this table was removed
	// when T25 fixed it: expunges now wait for the connection's backlog to
	// drain, not merely for the current command's tagged response. Removing the
	// entry rather than leaving it is the point — a triaged finding that stays
	// after the bug is gone turns this filter into a permanent blindfold, and
	// the regression is pinned by TestExpungeUpdateWaitsForPipelinedCommands.
}

// assertNoImaptestFailures fails the test on any complaint imaptest made that
// is not already a recorded finding.
//
// A failure here is our bug, not a broken container: we control both halves,
// which is the property that makes a first-party entry worth having. If a
// finding turns out to be imaptest asserting something stricter than the RFC
// requires, the resolution is a documented, triaged exception recorded in
// docs/INTEROP.md — never a silent widening of this filter.
func assertNoImaptestFailures(t *testing.T, out string) {
	t.Helper()
	var complaints []string
	seen := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !imaptestError.MatchString(line) {
			continue
		}
		// Only a line that opens a complaint is one. imaptest quotes the
		// offending response back, and a quoted FETCH carrying a literal spans
		// lines — every continuation still says "Error:" because imaptest
		// prefixes the whole quotation, and none of them match a triage pattern,
		// so counting them made one real finding read as hundreds. A run that
		// reported 1619 problems had twelve.
		rest, ok := complaintBody(line)
		if !ok {
			continue
		}
		if finding, ok := matchTriaged(rest); ok {
			seen[finding]++
			continue
		}
		complaints = append(complaints, line)
	}
	for finding, count := range seen {
		t.Logf("TRIAGED (%d occurrences): %s", count, finding)
	}
	if len(complaints) > 0 {
		t.Errorf("imaptest reported %d untriaged problem(s) against our server:\n%s",
			len(complaints), strings.Join(complaints, "\n"))
	}
	for _, summary := range imaptestSummaries(out) {
		t.Logf("imaptest %s: %d total, %d failed", summary.kind, summary.total, summary.failed)
		if summary.failed > 0 {
			t.Errorf("%d of %d imaptest %s failed", summary.failed, summary.total, summary.kind)
		}
	}
}

// complaintHeader matches the opening line of an imaptest complaint, which
// names the account and the simulated client that hit it:
//
//	Error: interop@example.test[40]: Referenced message expunged seq=7 uid=0: …
//
// Anything else carrying "Error:" is the quoted server response the complaint is
// about, continued across lines.
var complaintHeader = regexp.MustCompile(`(?i)^(error|panic|fatal)[^:]*:\s*\S+\[\d+\]:\s*(.*)$`)

// complaintBody returns the message of a complaint, and whether the line opens
// one at all.
func complaintBody(line string) (string, bool) {
	match := complaintHeader.FindStringSubmatch(line)
	if match == nil {
		return "", false
	}
	return match[2], true
}

func TestComplaintBody(t *testing.T) {
	tests := []struct {
		name string
		line string
		body string
		ok   bool
	}{
		{
			name: "complaint",
			line: "Error: interop@example.test[40]: Referenced message expunged seq=7 uid=0: * 7 EXPUNGE",
			body: "Referenced message expunged seq=7 uid=0: * 7 EXPUNGE",
			ok:   true,
		},
		{
			name: "case insensitive",
			line: "FATAL: interop@example.test[3]: connection lost",
			body: "connection lost",
			ok:   true,
		},
		{
			name: "quoted response continuation",
			line: `Error: Message-ID: <fixture-3@example.test>`,
		},
		{
			name: "summary text",
			line: "2 / 4 tests failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, ok := complaintBody(tt.line)
			if body != tt.body || ok != tt.ok {
				t.Fatalf("complaintBody(%q) = %q, %v; want %q, %v", tt.line, body, ok, tt.body, tt.ok)
			}
		})
	}
}

// matchTriaged reports whether a complaint is a known, recorded finding.
func matchTriaged(line string) (string, bool) {
	for _, entry := range triaged {
		if entry.pattern.MatchString(line) {
			return entry.finding, true
		}
	}
	return "", false
}

type imaptestSummary struct {
	kind          string
	total, failed int
}

var (
	imaptestGroupSummary   = regexp.MustCompile(`^(\d+) test groups:\s*(\d+) failed(?:,|$)`)
	imaptestCommandSummary = regexp.MustCompile(`^(base protocol|extensions):\s*(\d+)\s*/\s*(\d+) individual commands failed$`)
	imaptestLegacySummary  = regexp.MustCompile(`^(\d+)\s*/\s*(\d+)\s+tests?\s+failed(?:\s|$)`)
)

// imaptestSummaries accepts both the legacy "failed / total tests failed"
// report and current group/command summaries. Stress counters are not script
// summaries. Every nonzero count is a failure, even without an Error: complaint.
func imaptestSummaries(out string) []imaptestSummary {
	var summaries []imaptestSummary
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		var kind, totalText, failedText string
		if match := imaptestGroupSummary.FindStringSubmatch(line); match != nil {
			kind, totalText, failedText = "test groups", match[1], match[2]
		} else if match := imaptestCommandSummary.FindStringSubmatch(line); match != nil {
			kind, totalText, failedText = match[1]+" commands", match[3], match[2]
		} else if match := imaptestLegacySummary.FindStringSubmatch(line); match != nil {
			kind, totalText, failedText = "scripts", match[2], match[1]
		} else {
			continue
		}
		total, err := strconv.Atoi(totalText)
		if err != nil {
			continue
		}
		failed, err := strconv.Atoi(failedText)
		if err != nil {
			continue
		}
		summaries = append(summaries, imaptestSummary{kind: kind, total: total, failed: failed})
	}
	return summaries
}

func TestImaptestSummaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want []imaptestSummary
	}{
		{
			name: "current failing transcript without Error complaint",
			out:  "71 test groups: 19 failed, 0 skipped due to missing capabilities\nbase protocol: 47/457 individual commands failed\nextensions: 25/157 individual commands failed\n",
			want: []imaptestSummary{{kind: "test groups", total: 71, failed: 19}, {kind: "base protocol commands", total: 457, failed: 47}, {kind: "extensions commands", total: 157, failed: 25}},
		},
		{
			name: "current clean CRLF transcript",
			out:  " 71 test groups: 0 failed, 2 skipped due to missing capabilities\r\nbase protocol: 0/457 individual commands failed\r\nextensions: 0/157 individual commands failed\r\n",
			want: []imaptestSummary{{kind: "test groups", total: 71}, {kind: "base protocol commands", total: 457}, {kind: "extensions commands", total: 157}},
		},
		{name: "legacy failure", out: "2 / 4 tests failed", want: []imaptestSummary{{kind: "scripts", total: 4, failed: 2}}},
		{name: "legacy clean", out: "0 / 71 tests failed", want: []imaptestSummary{{kind: "scripts", total: 71}}},
		{name: "legacy with suffix", out: "2 / 4 tests failed (1 skipped)", want: []imaptestSummary{{kind: "scripts", total: 4, failed: 2}}},
		{name: "legacy singular", out: "1/1 test failed", want: []imaptestSummary{{kind: "scripts", total: 1, failed: 1}}},
		{name: "command failure despite zero groups", out: "71 test groups: 0 failed\nextensions: 1/157 individual commands failed", want: []imaptestSummary{{kind: "test groups", total: 71}, {kind: "extensions commands", total: 157, failed: 1}}},
		{name: "stress and unrelated counters", out: "Logins: 4\nSelects: 19\n47/457 commands\nno scripts failed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := imaptestSummaries(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("summaries = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("summary %d = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
