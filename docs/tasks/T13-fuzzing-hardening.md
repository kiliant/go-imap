# T13 — Fuzzing & hardening

**Agent:** `fuzz-hardening` · **Milestone:** M4 · **Depends on:** T01, T12

**Owns:** `**/*_fuzz_test.go`, `internal/imapwire/testdata/**`,
`interop/harness/adversarial/**`

## Threat model

**The server is untrusted.** A client may connect to an attacker-controlled
server — or merely a buggy one. Neither may crash, hang, or exhaust the memory of
the process using this library.

## Invariants to enforce

1. **No panics.** Any byte sequence yields an error; failures crossing the public
   client boundary are `*imap.Error`. Covers slice bounds, nil derefs, and
   integer overflow in literal-length arithmetic.
2. **No unbounded allocation.** Literal size, line length, list nesting depth and
   buffered untagged responses are all capped, and the cap is checked *before*
   allocating. `{4294967295}` must be rejected, not attempted.
3. **No hangs.** Every production network read observes a deadline. A server
   that opens a literal and stalls must time out.
4. **No desynchronisation.** A partially consumed body section either drains or
   invalidates the connection. Silent desync attributes responses to the wrong
   command — a correctness *and* a security bug.

## Work

- `Fuzz*` target per parser entry point, seeded from `testdata/` and from real
  interop captures. Corpus committed; crashers gitignored until minimised into a
  regression test.
- Adversarial fake server: truncated literals, literal length not matching the
  payload, responses carrying tags never sent, `BYE` mid-command, 10 MiB header
  lines, unterminated lists, 1000-deep nesting, `*` in impossible positions,
  NUL bytes in atoms, CR without LF.
- `go test -race` across everything touching the reader goroutine — the
  demultiplexer is the highest-risk code in the library.
- Memory-bound test: fetch a 200 MiB message, assert peak allocation stays flat.
  This is the regression test for the streaming guarantee.

## Also yours

- Assert wire tracing redacts `LOGIN` arguments and `AUTHENTICATE` payloads.
- Assert TLS verification is on by default.
- Assert post-`STARTTLS` capabilities are re-fetched, not carried over from
  cleartext.

## Escalation

Findings that require an API change go to `api-guardian` with the failing input.
Do not make the change yourself — you do not own those files.

## Done when

All fuzz targets have recorded 10-minute clean runs (human-approved campaign
duration, 2026-08-03). The adversarial suite,
module-wide `-race` run, memory-bound streaming regression, production read
deadline, and buffered-response limits pass. T15 owns promoting these established
checks into the 60-second PR fuzz smoke and nightly long-run CI jobs; CI file
ownership is not a T13 completion dependency.
