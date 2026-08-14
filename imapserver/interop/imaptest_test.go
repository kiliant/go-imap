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
	runtime := runtimeForTests(t)
	buildImage(t, runtime, imaptestImage, "testdata/imaptest")
	port := serverForClients(t)

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
	runtime := runtimeForTests(t)
	buildImage(t, runtime, imaptestImage, "testdata/imaptest")
	port := serverForClients(t)

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

// triaged are the complaints imaptest currently makes that are known, recorded
// findings rather than regressions. Each one is a real defect in this server,
// not a tool quirk, and each is written up in docs/INTEROP.md with the
// transcript evidence that identifies it.
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
	{
		regexp.MustCompile(`Invalid untagged input: \* \d+ EXPUNGE|Referenced message expunged`),
		"EXPUNGE delivered while a pipelined FETCH/STORE/SEARCH is still in " +
			"progress, which RFC 3501 section 7.4.1 forbids",
	},
	{
		regexp.MustCompile(`Keyword used without being in FLAGS`),
		"a keyword created by STORE is reported in FETCH without the mailbox's " +
			"FLAGS set having been re-announced",
	},
	{
		// imaptest asserts internally once its own view of the mailbox has
		// desynchronised, which is downstream of the first finding rather than
		// a separate one.
		regexp.MustCompile(`Raw backtrace|Panic:`),
		"imaptest's own assertion failure, downstream of the EXPUNGE finding",
	},
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
		if finding, ok := matchTriaged(line); ok {
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
	if total, failed, ok := imaptestTotals(out); ok {
		t.Logf("imaptest scripts: %d run, %d failed", total, failed)
		if failed > 0 {
			t.Errorf("%d of %d imaptest scripts failed", failed, total)
		}
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

// imaptestTotals reads the "x / y tests failed" summary imaptest prints after a
// scripted run, when it printed one.
func imaptestTotals(out string) (total, failed int, ok bool) {
	summary := regexp.MustCompile(`(\d+)\s*/\s*(\d+)\s+tests?\s+failed`)
	match := summary.FindStringSubmatch(out)
	if match == nil {
		return 0, 0, false
	}
	failed, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	total, err = strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, false
	}
	return total, failed, true
}
