# Maildir scale measurements

Issue #117 compares the checked-in `BenchmarkMaildirScale` fixtures against
`e00cbb2` (before these changes). Both runs used the same Apple M1 development
machine, local filesystem and Go 1.26.5 on darwin/arm64, with eight logical
execution threads. The baseline is one three-iteration sample per fixture;
the new results are the median of three three-iteration samples. The final
comparison ran without the concurrent test suites used during development.
These small samples are observations, not latency or capacity guarantees.

Run from `imapserver`:

```sh
go test ./maildir -run '^$' -bench BenchmarkMaildirScale -benchmem -benchtime=3x -count=3
```

The fixture and measured operation boundaries are unchanged. MOVE returns its
message to INBOX outside the timed region. Initialization and SELECT occur
before measurement. The short samples measure cached idle polls and ordinary
mutations; they do not include a periodic full checkpoint.

| Messages | Operation | Before ms/op | After ms/op | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1,000 | idle-poll | 45.71 | 1.13 | 959,973 | 25,621 | 18,332 | 438 |
| 1,000 | single-store | 65.59 | 45.73 | 9,314,306 | 61,869 | 161,294 | 674 |
| 1,000 | single-move | 80.66 | 59.11 | 9,790,869 | 137,957 | 161,807 | 1,033 |
| 20,000 | idle-poll | 915.60 | 1.12 | 19,272,909 | 25,621 | 360,404 | 438 |
| 20,000 | single-store | 694.37 | 46.82 | 198,932,128 | 216,765 | 3,201,502 | 669 |
| 20,000 | single-move | 626.66 | 67.72 | 203,820,938 | 1,039,069 | 3,202,012 | 1,047 |

At 20,000 messages, cached idle work is independent of live-message count in
these fixtures: read the normally empty `new/`, compare directory identity,
mtime and change time, and audit at most 16 stored files per mailbox. Reading
`new/` on every poll preserves delivery visibility even when a filesystem
coalesces timestamps. A changed directory still takes a full reconciliation;
unsupported in-place edits to `cur/` contents may be diagnosed on a later
rotating audit. The documented ownership contract already forbids those edits.

Mutations clone account maps and the affected mailboxes' pointer slices, replace
only changed message records, and persist compact changes. They still scan the
affected mailbox to resolve changes; they do not copy or serialize unaffected
message records. Unknown account/mailbox/message/tombstone fields survive.
At this size, STORE and MOVE temporary allocations fall by more than 99%.
Filesystem fsync latency remains part of every successful mutation.

Format 4 uses a full `.imap-state.json` checkpoint, numbered change files, a
durable `.imap-head.json`, and the prepared `.imap-journal.json`. The head
records the last committed sequence so a missing tail change fails closed.
Checkpointing occurs before another mutation after 64 changes, when aggregate
change data would exceed 8 MiB, or when approaching the 256 MiB storage budget.
A single change may exceed the soft 8 MiB checkpoint target. Checkpoint plus
changes conservatively bound reconstructed state; a shrinking transaction can
fall back to a complete journal if that conservative bound is too large.
Open also consolidates changes. These paths retain linear full-account costs.

Migration publishes a version-4 checkpoint before preparing any incremental
delivery transaction, so a crash cannot expose a version-3 checkpoint with
committed changes an older reader would ignore. Versions 1–3 and prepared older
journals migrate without changing UID/UIDVALIDITY. Checkpoint publication
precedes deleting covered changes. A generation tied to INBOX UIDVALIDITY keeps
retired history out of a missing-state reconstruction. Back up the complete
account directory; older readers refuse format 4.

Regression coverage includes lost middle/tail changes, lost heads, unsupported
versions, checkpoint interruptions, both persistence boundaries, generation
retirement, migration ordering, rollback isolation and cached delivery/removal.
Existing recovery fault matrices, bounded-history/QRESYNC, cancellation,
SELECT/update attachment, account concurrency and backend conformance remain
acceptance gates. The new change decoder has a fuzz target. Native/emulated
interoperability continues through the existing CI workflow.

The public API, 100 ms default polling interval, metadata/history limits and
small-account guidance are unchanged. Large accounts still need workload and
filesystem measurements, including checkpoints, changing-directory scans and
multiple concurrent sessions. These results do not establish a new capacity
envelope.
