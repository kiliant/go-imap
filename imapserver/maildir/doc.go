// Package maildir implements a persistent IMAP backend using Maildir message
// files and journalled account metadata.
//
// The server module's examples/maildir.go is a runnable server with account
// configuration, polling diagnostics and orderly shutdown. From imapserver,
// run it with: go run ./examples/maildir.go ./examples/config.go.
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
// Account state is version 3. It holds mailbox identities, UIDNEXT, message
// mappings, flags/keywords, modification sequences, metadata, subscriptions and
// a bounded expunge history. MaxTombstones defaults to 10000 per mailbox.
// Pruning persists the highest forgotten expunge mod-sequence. A QRESYNC
// request below that checkpoint receives every currently missing UID in the
// requested scope, as a conservative superset; live UIDs are never reported
// vanished. Known sequence/UID match samples restrict that set. At or above
// the checkpoint, retained history remains exact, including across restarts.
// Version 2 migrates to 3 without changing mailbox identities. Version 1 also
// supplies missing revision/modification-sequence fields as 1. Prepared older
// journals are recovered before migration. Older releases refuse version 3
// rather than ignore its history checkpoint. METADATA
// values that are valid UTF-8 are JSON strings; any other value, which RFC 5464
// permits, is an object {"base64": ...}, so binary octets survive. Unknown fields
// at root/account/mailbox/message/tombstone boundaries survive rewrites. Newer
// versions are refused without rewriting those files. Identity/root registry
// formats are version 1. State, journal, identity and root-registry files are
// limited to 256 MiB on both reads and writes. A mutation whose serialized
// journal exceeds that budget returns NO [LIMIT] before preparing a transaction,
// leaving the previous state usable. MaxMetadataValueBytes defaults to 65536
// octets and MaxMetadataEntries to 1024 per mailbox/server scope. A SETMETADATA
// exceeding either returns METADATA MAXSIZE or TOOMANY atomically, without a
// journal or partial update. Existing values remain readable after limits are
// lowered; deletions can reduce an over-limit entry count. Tombstones are
// pruned to their configured count before journal preparation.
//
// Mutations first fsync staged message data, then atomically publish and fsync
// .imap-journal.json. Recovery rolls this prepared transaction forward using
// idempotent filesystem actions, atomically publishes/fsyncs account state,
// then removes/fsyncs the journal. A successful mutation returns only after
// that sequence. Once preparation is durable, cancellation or an I/O error may
// still leave the operation committed. A later commit error disables that
// account until reopen/recovery; no subsequent reader observes partial state.
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
// Each current diagnostic identifies its account and, when known, mailbox.
// Successful polls clear that account's current failure. Callbacks run serially
// outside locks with a cancellation context, may retain their copied diagnostic,
// and must not call Close synchronously.
// Close stops polling and rejects new work, waits for operations/callbacks,
// then releases roots and locks, without waiting for idle session lifetimes.
//
// # Scale and measurement
//
// This backend targets small accounts: around 1000 live messages per account
// is the initial latency envelope for the default 100 ms poll interval.
// This is guidance, not a capacity guarantee. Polling still stats every stored
// file, and a mutation still clones and writes the whole account state.
// On a local Apple M1 filesystem, three one-operation samples at 20000 messages
// measured roughly 1 s per idle poll and 0.6 s per STORE or single-message MOVE.
// At that size, use a poll interval of several seconds and expect mutations to
// queue behind each other. Measure the intended filesystem and workload before
// choosing this backend for larger or mutation-heavy accounts. Message bytes
// stream during FETCH, but that does not remove metadata-write costs.
//
// BenchmarkMaildirScale provides 1000/20000-message idle-poll, STORE and MOVE
// fixtures; run go test -run '^$' -bench BenchmarkMaildirScale -benchtime=1x
// -count=3 in this package. Idle polls avoid a full-state clone, and MOVE keeps
// unaffected messages in linear time. Incremental persistence and a file index
// remain future optimisations rather than part of the v1 contract.
package maildir
