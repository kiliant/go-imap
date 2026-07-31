# T04 — Authentication & SASL

**Agent:** `client-core` · **Milestone:** M1 · **Depends on:** T03

**Owns:** `imapclient/auth.go`, `internal/imapsasl/**`

## Goal

Every authentication mechanism a real deployment uses, implemented on stdlib
crypto only.

## Mechanisms

| Mechanism | RFC | Note |
|---|---|---|
| `LOGIN` command | 3501 | Must refuse when `LOGINDISABLED` is advertised |
| PLAIN | 4616 | Refuse over cleartext unless explicitly permitted |
| LOGIN (SASL) | — | Legacy, still deployed |
| CRAM-MD5 | 2195 | Legacy, weak, still common — support, do not recommend |
| SCRAM-SHA-1 | 5802 | |
| SCRAM-SHA-256 | 7677 | |
| SCRAM-SHA-*-PLUS | 5802, 7677 | Channel binding via `tls.ConnectionState.ExportKeyingMaterial` |
| XOAUTH2 | — | De-facto; Gmail, Outlook |
| OAUTHBEARER | 7628 | The standardised successor |

Plus `SASL-IR` (RFC 4959) initial-response optimisation when advertised, with
correct handling of the empty-initial-response `=` encoding — a common bug.

## Design

- A SASL mechanism is a struct with a `Next([]byte) ([]byte, error)` step
  function, not an exported interface. Callers supply custom mechanisms through
  a function field in the options struct.
- Mechanism selection: prefer the strongest the server advertises; allow an
  explicit override. Document the preference order.
- Credentials never appear in logs, traces, or errors. Assert this in a test —
  `AUTHENTICATE` payloads and `LOGIN` arguments both.

## Security requirements

- `LOGINDISABLED` → refuse *before* transmitting credentials, not after.
- PLAIN and LOGIN over a non-TLS connection → refuse unless the caller has
  explicitly opted in via a documented option.
- SCRAM: verify the server's final signature. Skipping this silently defeats the
  mutual authentication that is the reason to use SCRAM.
- On authentication failure, surface the `AUTHENTICATIONFAILED` /
  `AUTHORIZATIONFAILED` / `PRIVACYREQUIRED` response codes (RFC 5530) rather than
  a generic error — callers need to distinguish "wrong password" from "use TLS".
- Re-issue `CAPABILITY` after authentication; the post-auth list differs.

## Done when

Authenticates against Dovecot with PLAIN, CRAM-MD5, SCRAM-SHA-256, and against
Stalwart with OAUTHBEARER. Redaction test passes. Downgrade refusals tested.
