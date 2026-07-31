# T04 — Authentication & SASL

**Agent:** `client-core` · **Milestone:** M1 · **Depends on:** T03

**Owns:** `imapclient/auth.go`, `internal/imapsasl/**`, `internal/saslprep/**`,
`internal/unicodenorm/**` (fuzz targets in those trees are T13-owned once landed)

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

## Credential preparation (SASLprep, RFC 4013)

RFC 5802 *requires* SASLprep for SCRAM and RFC 4616 recommends it for PLAIN, but
it is **off by default**, exposed as `AuthenticateOptions.PrepareCredentials`.
This is not a shortcut — it is what the matrix measured. Dovecot 2.4.3 and
Stalwart 0.11.8 both store and compare raw password octets, so preparing
unconditionally would *break* authentication against the two most relevant
servers for any password normalisation changes. A spec-compliant default that
fails against real servers is the wrong default; the option lets a caller talk to
a server that does prepare at enrollment.

Rules:

- Prepare once, before any mechanism is constructed, so a prohibited code point
  or bidi violation aborts before anything reaches the wire. Assert the
  zero-bytes-written property in a test.
- Applies to username *and* password for the password mechanisms: PLAIN, SASL
  LOGIN, CRAM-MD5 and the four SCRAM variants. Never to bearer tokens
  (XOAUTH2, OAUTHBEARER) or to a caller-supplied mechanism, whose credentials
  the client never sees.
- CRAM-MD5's inclusion is deliberate. RFC 2195 predates stringprep and mandates
  nothing, but exempting one password mechanism from an explicit opt-in would be
  undiscoverable for callers.
- Redaction must cover the *prepared* forms as well as the caller's input. The
  prepared octets are what reached the server, and the two differ by construction
  for exactly the inputs this option exists to handle.
- `Client.Login` does not prepare and says so: it has no options struct, and
  adding a parameter would break every caller.

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
Stalwart with OAUTHBEARER. Redaction test passes, including the prepared forms.
Downgrade refusals tested. `PrepareCredentials` is proven end to end by
`interop/saslprep`, whose 2×2 over raw-U+00B5 and NFKC-U+03BC stored accounts is a
hard assertion rather than an observation of server behaviour.
