// Package maildir implements a persistent IMAP backend using Maildir message
// files and journalled account metadata.
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
// and atomically rename them into new/. Symlinks and nonregular message files
// are refused. Account-root handles constrain file access; Go 1.24 uses a
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
// APPEND preserves all bytes exactly.
// new/ ingestion normalizes LF to CRLF without duplicating existing CRLF and
// preserves delivery mtime. INTERNALDATE is the stored file mtime. Message
// size, sections, ENVELOPE, BODYSTRUCTURE and SEARCH use the same stored bytes.
//
// .imap-root.json registers previously initialized accounts. Each account's
// .imap-identity.json is a monotonic UIDVALIDITY allocator, separate from
// recoverable .imap-state.json. Missing account state reconstructs messages
// from cur/ and new/ with fresh UIDVALIDITY; keywords, metadata, subscriptions
// and tombstones lost with that state cannot be recovered. Missing registered
// account directories or allocators, or a missing existing root registry, fail
// closed rather than risk reusing an identity. Preserve the complete root in
// backups; erasing the entire root is provisioning a different store.
//
// Account state is version 2. It holds mailbox identities, UIDNEXT, message
// mappings, flags/keywords, modification sequences, metadata, subscriptions and
// all expunge tombstones. History is not pruned, so QRESYNC remains exact across
// restart and state grows with expunges. Version 1 uses the same schema but may
// omit revision/modification-sequence fields, which migrate to 1. METADATA
// values that are valid UTF-8 are JSON strings; any other value, which RFC 5464
// permits, is an object {"base64": ...}, so binary octets survive. Unknown fields
// at root/account/mailbox/message/tombstone boundaries survive rewrites. Newer
// versions are refused without rewriting those files. Identity/root registry
// formats are version 1. Metadata files are limited to 256 MiB when read.
//
// Mutations first fsync staged message data, then atomically publish and fsync
// .imap-journal.json. Recovery rolls this prepared transaction forward using
// idempotent filesystem actions, atomically publishes/fsyncs account state,
// then removes/fsyncs the journal. A successful mutation returns only after
// that sequence. Once preparation is durable, cancellation or an I/O error may
// still leave the operation committed. A later commit error disables that
// account until reopen/recovery; no subsequent reader observes partial state.
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
// operations within an account are serialized, including streaming FETCH.
// Polling and SELECT share that lock: every delivery is in the atomic snapshot
// or a subsequent revision-chained update, never missed between them. Poll
// errors remain observable through LastPollError and optional OnPollError.
// Close stops polling and rejects new work, waits for operations/callbacks,
// then releases roots and locks, without waiting for idle session lifetimes.
package maildir
