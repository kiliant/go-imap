// Package maildir implements a persistent IMAP backend using Maildir message
// files and journalled account metadata.
//
// The server module's examples/maildir.go is a runnable server with account
// configuration, polling diagnostics and orderly shutdown. From imapserver,
// run it with: go run ./examples/maildir.go ./examples/config.go.
//
// RENAME INBOX moves its messages to a newly created mailbox, leaving the
// empty INBOX and its inferiors in place. A destination beneath INBOX is
// refused with NO [CANNOT].
//
// # Storage contract
//
// This backend supports local filesystems on Linux and macOS that implement
// atomic rename, fsync of files/directories, and advisory flock. Network and
// userspace filesystems with weaker semantics are not supported. Open fails
// explicitly on unsupported platforms or when another process owns the root.
// Locks are released by the OS when the process exits, including an unclean exit.
// Keep each account's staging and mailbox directories on the same filesystem.
//
// The root and its structural directories belong exclusively to this backend.
// Do not rename them, edit cur/ files, or restore an older live snapshot while
// it is open. External delivery may write regular files in a mailbox's tmp/
// and atomically rename them into new/. Invalid or unreadable entries in new/
// are skipped and left for repair; valid deliveries continue. Initial skips
// appear in PollFailures. Polling reports them through OnPollFailure.
// Symlinks and nonregular stored messages are refused. Account-root handles
// constrain file access; Go 1.24 uses a
// checked-path rename fallback because os.Root.Rename was added in Go 1.25.
// That fallback also requires the root's ancestor paths to remain unchanged
// while open, in addition to structural ownership. Passwords are
// configuration only, never persistent storage data.
//
// Accounts are lowercase SHA256(username bytes) directories. Beneath each,
// mailboxes/<escaped hierarchy>/ contains cur/, new/, tmp/. Segment escaping
// is compatible with cmd/imapcli: uppercase percent-hex encodes %, /, controls,
// and an initial dot or reserved cur/new/tmp name; an empty segment is %.
// This is not Maildir++. Filename flags are D F R S T, sorted in that order;
// keywords live in metadata, and applicable keywords remain registered after
// their final message reference disappears.
//
// Mailbox and hierarchy segments must retain their exact directory spelling.
// CREATE/RENAME reject case or Unicode-normalization aliases on filesystems
// that equate those names, before preparing a transaction. Distinct names are
// allowed on filesystems that distinguish them. Open refuses aliased existing
// state; it does not merge mailbox identities or rewrite that state.
//
// RECENT ownership is selection-specific. Pending delivery/APPEND/COPY state
// is durable; a read-write selection atomically claims it while EXAMINE leaves
// it available to a later read-write selection. Old records lacking explicit
// state are conservatively considered recent. Recent flags are never stored as
// client-controlled permanent flags or filename flags.
//
// APPEND preserves all bytes exactly.
// new/ ingestion normalizes LF to CRLF without duplicating existing CRLF and
// preserves delivery mtime. INTERNALDATE is the stored file mtime. Message
// size, sections, ENVELOPE, BODYSTRUCTURE and SEARCH use the same stored bytes.
//
// .imap-root.json registers previously initialized accounts. Each account's
// .imap-identity.json is a monotonic UIDVALIDITY allocator, seeded from Unix
// time for new accounts and separate from
// recoverable .imap-state.json. Missing account state reconstructs messages
// from cur/ and new/ with fresh UIDVALIDITY; keywords, metadata, subscriptions
// and tombstones lost with that state cannot be recovered. Missing registered
// account directories or allocators fail that account closed rather than risk
// reusing an identity. Open fails by default; IsolateAccountErrors instead
// keeps healthy accounts available and rejects authentication to failed ones
// until storage is repaired and the backend reopened. It never reprovisions
// a registered account or hides its failure. Root registry and locking errors
// always fail Open. Preserve the complete root in
// backups; erasing the entire root is provisioning a different store.
// Interrupted first-time setup can resume when only the root lock, an empty
// lost+found directory and unpublished atomic-write temporary files remain.
// Recognized atomic-write temporaries are removed under the root lock.
//
// A v1 minor release may migrate the storage format forward, preserving
// mailbox/message identities and unknown fields at supported record boundaries
// and recovering prepared older journals. Every bump is called out in the
// changelog. Older readers refuse unsupported versions rather than risk stale
// or corrupt data. Downgrade compatibility is not promised: rollback requires
// restoring a complete backup made before migration with the backend closed.
// These documented migrations are compatible under the v1 stability promise.
//
// Account state is version 4. It holds mailbox identities, UIDNEXT, message
// mappings, flags/keywords, modification sequences, metadata, subscriptions and
// a bounded expunge history. MaxTombstones defaults to 10000 per mailbox.
// Pruning persists the highest forgotten expunge mod-sequence. A QRESYNC
// request below that checkpoint receives every currently missing UID in the
// requested scope, as a conservative superset; live UIDs are never reported
// vanished. Known sequence/UID match samples restrict that set. At or above
// the checkpoint, retained history remains exact, including across restarts.
// Versions 1, 2 and 3 migrate to 4 without changing mailbox identities; version
// 1 supplies missing revision/modification-sequence fields as 1. Prepared older
// journals are recovered before migration. Older releases refuse version 4
// rather than serve a stale checkpoint. METADATA
// values that are valid UTF-8 are JSON strings; any other value, which RFC 5464
// permits, is an object {"base64": ...}, so binary octets survive. Unknown fields
// at root/account/mailbox/message/tombstone boundaries survive rewrites. Newer
// versions are refused without rewriting those files. Identity/root registry
// formats are version 1. State, journal, change, head, identity and root-registry
// files are limited to 256 MiB on reads and writes. Checkpoint plus pending
// changes conservatively bound reconstructed state to the same budget. A
// mutation exceeding either budget returns NO [LIMIT] before preparing a transaction,
// leaving the previous state usable. MaxMetadataValueBytes defaults to 65536
// octets and MaxMetadataEntries to 1024 per mailbox/server scope. A SETMETADATA
// exceeding either returns METADATA MAXSIZE or TOOMANY atomically, without a
// journal or partial update. Existing values remain readable after limits are
// lowered; deletions can reduce an over-limit entry count. Tombstones are
// pruned to their configured count before journal preparation.
//
// Mutations first fsync staged message data, then atomically publish and fsync
// .imap-journal.json. Recovery rolls this prepared transaction forward using
// idempotent filesystem actions, atomically publishes/fsyncs a numbered
// .imap-change-<sequence>.json containing changed message records and mailbox
// headers, updates/fsyncs .imap-head.json, then removes/fsyncs the journal.
// The durable head detects missing change files, including a lost final change,
// and fails that account closed. A successful mutation returns only after
// that sequence. Once preparation is durable, cancellation or an I/O error may
// still leave the operation committed. A later commit error disables that
// account until reopen/recovery; no subsequent reader observes partial state.
// Before another mutation after 64 changes or 8 MiB of change data, and on
// Open, the backend consolidates changes into .imap-state.json, fsyncs it,
// then retires covered changes. A crash leaves the old checkpoint plus its
// changes, or the new checkpoint with harmless covered changes. A generation
// tied to the account's INBOX identity separates missing-state reconstruction
// from its retired change history. Back up the complete account directory.
// If both names of a journalled cur-to-cur message rename have vanished,
// recovery records that UID as expunged with a newer modification sequence,
// retaining UIDVALIDITY and UIDNEXT. Missing staged APPEND/COPY bytes and
// structural/identity corruption remain errors requiring operator repair;
// IsolateAccountErrors can keep other accounts serving during that repair.
//
// APPEND and COPY stage bytes before publishing their new mappings. MOVE's
// destination mappings, source removal and tombstones share one transaction.
// STORE journals filename flag changes with metadata; EXPUNGE journals removals
// with tombstones. RENAME journals the hierarchy relocation through a marked
// staging directory, including merges into existing hierarchy placeholders.
// RENAME INBOX moves messages to a fresh mailbox while retaining INBOX's
// identity and children and publishing removals to its existing selections.
//
// PollInterval bounds idle time before the next delivery scan; ingestion,
// another active operation, scheduling and a callback add to that delay. All
// state changes within an account are serialized. FETCH opens one message at
// a time under the account lock, then releases it for analysis and streaming;
// a slow client does not block other sessions or delivery polling. An open
// source remains readable across a concurrent rename or expunge. Later UIDs
// that vanish before opening are skipped. Close still waits for streaming.
// Polling and SELECT share the state lock: every delivery is in the atomic
// snapshot or a subsequent revision-chained update, never missed between them. Poll
// errors remain observable through PollFailures and optional OnPollFailure.
// Unchanged cur/ and new/ directory identities, mtimes and change times allow
// cached polls to skip cur/ directory reads and account-wide message stats.
// Every poll still reads new/ so coalesced filesystem timestamps cannot hide
// deliveries, and audits up to 16 stored files per mailbox in rotation. Unsupported edits
// to existing cur/ contents may therefore be diagnosed on a later poll;
// deliveries and unlinks invalidate the directory cache immediately.
// Each current diagnostic identifies its account and, when known, mailbox.
// Successful polls clear that account's current failure. Callbacks run serially
// outside locks with a cancellation context, may retain their copied diagnostic,
// and must not call Close synchronously.
// Close stops polling and rejects new work, waits for operations/callbacks,
// then releases roots and locks, without waiting for idle session lifetimes.
//
// # Scale and measurement
//
// The default 100 ms poll interval and small-account guidance remain unchanged.
// Cached idle polls inspect directory stamps and at most 16 files per mailbox;
// ordinary mutations copy the affected mailboxes' pointer slices and persist
// changed records, rather than cloning or rewriting every message. Changed
// directory scans and periodic checkpoints still require linear work, and
// mutations remain serialized per account. Measure the intended filesystem and
// workload before choosing this backend for larger or mutation-heavy accounts.
//
// BenchmarkMaildirScale provides 1000/20000-message idle-poll, STORE and MOVE
// fixtures; run go test -run '^$' -bench BenchmarkMaildirScale -benchmem
// -benchtime=3x -count=3 in this package. docs/MAILDIR-SCALE.md records the #117
// baseline, comparison and remaining costs. Measurements are observations,
// not latency or capacity guarantees.
package maildir
