# T13 — Fuzzing & hardening

**Agent:** `fuzz-hardening` · **Milestone:** M4 · **Depends on:** T01, T12 ·
**Status:** blocked

**Owns:** `**/*_fuzz_test.go`, `internal/imapwire/testdata/**`,
`interop/harness/adversarial/**`

## Threat model

**The server is untrusted.** A client may connect to an attacker-controlled
server — or merely a buggy one. Neither may crash, hang, or exhaust the memory of
the process using this library.

## Invariants to enforce

1. **No panics.** Any byte sequence yields an `*imap.Error`. Covers slice bounds,
   nil derefs, and integer overflow in literal-length arithmetic.
2. **No unbounded allocation.** Literal size, line length, list nesting depth and
   buffered untagged responses are all capped, and the cap is checked *before*
   allocating. `{4294967295}` must be rejected, not attempted.
3. **No hangs.** Every read observes a deadline. A server that opens a literal
   and stalls must time out.
4. **No desynchronisation.** A partially consumed body section either drains or
   invalidates the connection. Silent desync attributes responses to the wrong
   command — a correctness *and* a security bug.

## Work

- `Fuzz*` target per parser entry point, seeded from `testdata/` and from real
  interop captures. Corpus committed; crashers gitignored until minimised into a
  regression test.
- Adversarial fake server: truncated literals, literal length not matching the
  payload, responses carrying tags never sent, `BYE` mid-command, 10 MB header
  lines, unterminated lists, 1000-deep nesting, `*` in impossible positions,
  NUL bytes in atoms, CR without LF.
- `go test -race` across everything touching the reader goroutine — the
  demultiplexer is the highest-risk code in the library.
- Memory-bound test: fetch a 200 MB message, assert peak allocation stays flat.
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

All fuzz targets run 30 minutes clean. The adversarial suite passes. `-race`
clean across the module. The memory-bound test passes. CI runs a 60-second fuzz
smoke per target on every PR, plus a nightly long run.
