# T08 — Extensions group A: core modern

**Agent:** `extensions` · **Milestone:** M2 · **Depends on:** T07

**Owns:** `imapclient/ext_a_*.go`

Runs in parallel with T09. Verify every RFC number against
`docs/RFC-COVERAGE.md` before implementing — not from memory.

## Scope

| Capability | RFC | Notes |
|---|---|---|
| UIDPLUS | 4315 | `APPENDUID`, `COPYUID`, `UIDNOTSTICKY`; `UID EXPUNGE` |
| MOVE | 6851 | `MOVE`, `UID MOVE` |
| ESEARCH | 4731 | `MIN`, `MAX`, `ALL`, `COUNT` return options |
| SEARCHRES | 5182 | The `$` marker for the last search result |
| LIST-EXTENDED | 5258 | Selection/return options, multiple patterns |
| LIST-STATUS | 5819 | `STATUS` inline in `LIST` responses |
| SPECIAL-USE | 6154 | `\Drafts`, `\Sent`, `\Junk`, `\Trash`, `\Archive`, `\All`, `\Flagged` |
| CREATE-SPECIAL-USE | 6154 | `USE` parameter on `CREATE` |
| CHILDREN | 3348 | `\HasChildren`, `\HasNoChildren` |
| WITHIN | 5032 | `OLDER`, `YOUNGER` search keys |
| ID | 2971 | Client/server identification exchange |

## Requirements

- **Fallbacks** where the capability is absent, documented as emulated including
  the loss of atomicity:
  - `MOVE` → `COPY` + `STORE \Deleted` + `UID EXPUNGE`/`EXPUNGE`. Not atomic;
    say so in the doc comment.
  - `ESEARCH` `MIN`/`MAX`/`COUNT` → compute client-side from a plain `SEARCH`.
  - `LIST-EXTENDED` → multiple plain `LIST` calls.
  - `SPECIAL-USE` absent → fall back to `XLIST` where advertised (Gmail), else
    name heuristics, and make clear in the API that the result is a guess.
- **UIDPLUS is the one that pays.** `APPENDUID`/`COPYUID` let a client know the
  new UID without re-searching. Without it, document the race: another client may
  append between your `APPEND` and your `SEARCH`.
- `SEARCHRES` `$` is stateful per-connection and interacts with pipelining.
  Handle the case where an intervening command invalidates it.
- `LIST-EXTENDED` extends the T05 options struct with fields — it must not
  require a new method. If it does, T05's design was wrong: escalate rather than
  adding one.

## Done when

Each capability verified against **two** independent servers (Dovecot + Stalwart
for most; check `docs/INTEROP.md` profiles). Fallback paths tested against
GreenMail, which lacks several. Coverage rows updated to `verified`.
