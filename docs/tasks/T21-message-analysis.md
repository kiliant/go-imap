# T21 — Message analysis

**Agent:** `wire-protocol` · **Milestone:** M6 · **Depends on:** T16 approved,
v1.0 tagged

**Owns:** `internal/imapmessage/**`

## Why this is startable before the abstraction is settled

Nothing here depends on the backend abstraction in `SERVER-DESIGN.md` §2. This is
message content and the RFCs that describe it. Together with T18 it is the bulk
of the server project, and both can proceed while §2 is under review.

## What this is

`SERVER-DESIGN.md` §0's third tier: the parts that exist in **neither** direction
today, because a client never needed them. These are not mirrors of client code —
there is nothing to reverse.

A client *receives* an ENVELOPE and *asks* a SEARCH. A server must *construct*
the first and *answer* the second.

## Two subsystems, deliberately separated

They are separated because their inputs differ, and conflating them was a defect
in the first revision of the design.

### 1. Message analysis — a pure function of RFC 5322 bytes

**BODYSTRUCTURE generation.** The hard one. Requires exact octet counts and text
line counts per part, agreeing with the raw bytes byte-for-byte, including the
CRLF convention the message was stored in. A count that is off by one is a
client-visible corruption that only shows up on large or unusual messages.

**ENVELOPE generation.** RFC 5322 address-list parsing including group syntax.
Must **reproduce** malformed real-world headers rather than reject them: a server
that refuses to describe a message a client already accepted is worse than one
that describes it approximately. This is the opposite of the parser's usual
posture and needs saying out loud.

**Section extraction.** `BODY[HEADER.FIELDS (...)]<partial>` and friends, byte-exact,
with correct offsets into the original. Must stream — a `BODY[]` of a 200 MiB
message cannot be materialised, matching the contract
`imap.FetchDataBodySection.Literal` already has on the client side.

### 2. Search evaluation — bytes *plus* metadata

`imap.SearchCriteria` becomes a predicate. Its input is **not** just the message:
flags, UID, sequence position, INTERNALDATE, RFC822.SIZE and (under CONDSTORE)
MODSEQ are mailbox metadata that the stored bytes do not contain. So the API is
`(message, metadata) → bool`, not `(message) → bool`.

Also required: `CHARSET` handling, IMAP substring-matching semantics (which are
not Go's `strings.Contains` on raw bytes for non-ASCII), and date comparison
against both INTERNALDATE and header dates, which are different questions.

**This package is a helper, not a policy.** Per `SERVER-DESIGN.md` §5, the
backend owns selecting and enumerating matches; simple backends call this
evaluator per message, and indexed backends translate criteria into a native
query and never call it. Do not design it so that only the first kind can work —
no global state, no assumption that it sees every message in a mailbox.

## Standard library only

`net/mail`, `mime` and `mime/multipart` cover part of the analysis and none of
the byte-exactness. Zero dependencies still holds; expect to do the counting by
hand rather than by reusing a parser that discards offsets.

## Fuzz targets

Standing rule, and this package is a prime candidate: it parses attacker-supplied
message content. A message arrives by APPEND from an authenticated user, or by
delivery from anywhere at all.

Bar unchanged: no panic, no hang, no unbounded allocation. Corpus starts from the
interop fixtures and the client's existing body-structure testdata, which is
already full of real-world malformation.

An additional property worth fuzzing: **generation and parsing must agree.**
Generate a BODYSTRUCTURE from bytes, encode it, parse it back with
`internal/imapcodec`, and compare. That closes the loop against T18 and catches
the counting errors unit tests miss.

## Non-negotiables

- `internal/imapmessage` stays internal — API-STABILITY §6.
- Streaming, not buffering, for section extraction.
- Malformed input produces a best-effort description, never an error that makes a
  stored message unfetchable. A message that cannot be described is still a
  message the user must be able to download.

## Done when

- ENVELOPE and BODYSTRUCTURE are generated for every message in the interop
  fixture set, and round-trip through `internal/imapcodec` to an equal value.
- Section extraction is byte-exact against the fixtures, verified for the
  partial, header-field and MIME-header forms.
- `imap.SearchCriteria` evaluates correctly over a metadata-bearing message, with
  a test per criterion type — the criteria tree is open-ended, so a table-driven
  test that enumerates the concrete types and fails on an unhandled one is the
  gate that keeps this complete as new criteria are added.
- Fuzz targets green over a recorded campaign, per T13's policy.
