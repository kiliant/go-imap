# T28 — imapcli backup mode

**Agent:** `client-core` · **Milestone:** post-M6 (root `v1.2.0`) · **Depends on:** — (uses only the released `imap` and `imapclient` API)

**Owns:** `cmd/imapcli/**`, `interop/imapcli/**` (new), the "Command-line
tool" section of `README.md`, and the `cmd/imapcli` entries in `CHANGELOG.md`

## Why this exists

`cmd/imapcli` can browse a mailbox but cannot copy one. Its first real user
wants unattended full backups of two accounts — mailbox.org (Dovecot) and a
self-hosted Stalwart — into restic. Restic supplies the history, deduplication
and encryption; what is missing is a tool that keeps an exact, current local
copy of every mailbox and that can be left to run from launchd or cron.

It is also the library's first consumer that streams every message of an
account to disk. That exercises the FETCH streaming path harder than any
test in the tree.

## Design

**One mode, one flag.** `-backup DIR` turns the command into a mirror of the
whole account under `DIR`. It is mutually exclusive with `-mailbox`, `-uid`
and `-subscribed`. The session stays read-only, as in every other mode:
mailboxes are opened with EXAMINE and bodies are fetched with `BODY.PEEK[]`.
A backup must never mark mail as read.

**On-disk layout: Maildir, one directory per mailbox, nested like the server
hierarchy.**

```
DIR/
  .imapcli-backup.json          account marker: which addr/user owns DIR
  .imapcli-backup.lock          held for the duration of a run
  INBOX/
    .imapcli-mailbox.json       mailbox name, UIDVALIDITY, keywords
    cur/  new/  tmp/
    cur/V1700000000.U42:2,FS    one file per message
  Archive/
    2024/ …
```

- Restic stores a message once. A file is written once and never rewritten.
  A flag change renames it, which restic deduplicates for free.
- Any Maildir reader (mutt, notmuch, mbsync, Dovecot) can open the backup
  directly. That is the restore path. Uploading mail back (`APPEND`) would end
  the tool's read-only promise, so it is out of scope.
- Message bytes are exactly `BODY[]`, CRLF included, not converted to LF.
  A backup is a copy, and the bytes are what a later APPEND needs.
- The file's mtime is the message's INTERNALDATE. Maildir tools read the
  received date from mtime, and restic preserves it.
- The file name is `V<uidvalidity>.U<uid>:2,<flags>`. The standard info flags
  are `D` \Draft, `F` \Flagged, `R` \Answered, `S` \Seen and `T` \Deleted.
  Every other flag except \Recent, keywords included, goes in the mailbox state
  file. So no flag is lost.
- Mailbox names are split on the server's own delimiter. Each segment is made
  safe for a path: `%`, `/` and control characters are percent-escaped, and a
  segment that would be `.`, `..`, start with `.`, or equal `cur`, `new` or
  `tmp` has its first character escaped. An empty segment is written as `%`.
- The state file records the exact mailbox name. If a different mailbox maps
  to a directory that already holds another mailbox's state, that mailbox
  fails loudly instead of the two being merged silently. This can happen on
  case-insensitive or normalisation-insensitive file systems such as APFS.

**Synchronisation, per mailbox:**

1. `EXAMINE`. If UIDVALIDITY differs from the state file, every local message
   with the old value is stale. They are removed once the new ones are written.
2. `UID FETCH 1:* (UID FLAGS)` lists what exists. Its result is the
   authority for this run.
3. Messages on the server but not on disk are fetched in batches with
   `UID FETCH <set> (UID INTERNALDATE RFC822.SIZE BODY.PEEK[])`. Each body
   streams into `tmp/`, is fsynced, gets its mtime, and is renamed into `cur/`.
   An interrupted run therefore never leaves a partial message where restic
   would snapshot it.
4. Files whose flags changed are renamed. Files for messages that are gone,
   or that carry a stale UIDVALIDITY, are removed. Restic's snapshots are the
   history, so the local copy mirrors the server.
5. The state file is rewritten atomically and the directories are fsynced.

Mailboxes that no longer exist on the server lose their local copy. This
happens only after a run in which LIST and every mailbox succeeded, so a
partial failure never prunes anything. Files the tool did not write are left
alone.

**Unattended operation.**

- `-pass-file PATH` (`IMAP_PASS_FILE`) and `-pass-cmd CMD` (`IMAP_PASS_CMD`,
  run with `/bin/sh -c`) supply the password without a terminal. For example,
  `security find-generic-password -w …` reads it from the macOS keychain. At
  most one password source may be given.
- `-timeout` remains an overall limit, but in backup mode it defaults to none.
  A stalled connection is caught by the client's read timeout instead, which
  only fires when no data arrives.
- A run holds an exclusive lock on `DIR` (`flock` where available). A second
  run fails immediately rather than racing the first.
- `-exclude PATTERN` (repeatable; `*` and `?` globs over the full mailbox
  name) skips mailboxes, for example `-exclude 'Junk' -exclude 'Trash/*'`.
- COMPRESS=DEFLATE is used when the server offers it (`-compress=false`
  disables it).
- Exit status 0 means every mailbox was backed up. Status 1 means something
  failed; the failures are listed on stderr. The other mailboxes are still
  backed up. Status 2 is a usage error.

## Found while doing it

A defect found here is fixed in its own commit, as in T26, even in a file this
task does not own. If it touched the exported surface, it would go through the
normal API review.

- **Fixed: a streaming FETCH of several bodies raced.** Once a body literal
  had been read, its `LiteralReader` still touched the decoder's
  pending-literal interlock on every later `Read` (the EOF read `io.Copy`
  always makes) and on `Discard` (what `Close` does). By then the reader
  goroutine is already opening the next literal. Only `-race` showed it, and
  only with more than one body per command, which no test had. The fix is in
  `internal/imapwire/literal.go`: a reader releases the interlock once and
  then never touches the decoder again. A zero-length literal never holds it.
  Regression tests: `internal/imapwire/literal_release_test.go`.
- **Fixed: an empty body was handed over before its response was complete.**
  When `BODY[]` is NIL or a zero-length literal, there is nothing to stream.
  The response was still delivered at the body, while the reader goroutine
  went on appending the following items (UID, FLAGS, …) to the same map. That
  is a data race, and at worst a fatal concurrent map access, for any caller.
  A backup meets it on a message the server cannot read. Such a response is
  now delivered once it is complete (`internal/imapcodec/fetch.go`,
  `imapclient/fetch.go`). Regression test:
  `imapclient/fetch_empty_body_test.go`.
- **imapclient's default SASL choice sends a password as an OAuth token.**
  With no options, `Client.Authenticate` ranks OAUTHBEARER and XOAUTH2 above
  PLAIN and uses the password as the token. Stalwart offers OAUTHBEARER and
  PLAIN by default, so a correct password is refused. imapcli now picks a
  password mechanism itself. Changing the library's default is a behaviour
  change on the frozen v1 surface and is left to the maintainer.
- **Stalwart 0.11.8 hides new mailboxes from other sessions.** When a session
  CREATEs a mailbox, APPENDs to it and then EXAMINEs or SELECTs another
  mailbox, other sessions do not list the new mailbox while that session stays
  open. A backup can therefore miss a just-created folder until a later run.
  It cannot lose one already backed up, because only a deletion removes
  anything. The interop test changes server state in sessions that log out,
  as a mail client does.
- **Open: FETCH items after a streamed body are written to a map the caller
  already holds.** `FetchCommand.Next` hands over the `FetchMessageData` as
  soon as a non-empty body literal starts. The reader goroutine appends the
  items the server sends after the literal to the same `Items` map once the
  caller has consumed it. A caller that reads `Items` between consuming the
  literal and the next `Next` races with that write, and the documentation
  does not say so. The empty-body case above is fixed. This one is inherent
  to streaming, so the fix is documentation on `FetchCommand.Next`. That
  belongs to the owner of `imapclient/fetch.go`. T28 reads trailing items
  only after the following `Next` (or `io.EOF`), which the channel hand-off
  orders correctly.

## Out of scope

- Restore (`APPEND`), OAuth, and CONDSTORE/QRESYNC-based incremental flag
  sync. The full `UID FLAGS` listing is needed for deletion detection anyway,
  and costs a few megabytes for a 100 000-message mailbox.
- Any change to the `imap` or `imapclient` API. This task is a `main` package.

## Done when

- Scripted fake-server tests cover:
  - a first run, an incremental run (new message, flag change, expunge,
    UIDVALIDITY change, deleted mailbox) and a no-op run;
  - a server that sends UID after the body literal;
  - a mailbox-name collision, a foreign account marker, a held lock and
    password files and commands.
- `interop/imapcli` builds the real binary and runs it against Dovecot and
  Stalwart. It checks byte-exact content, flags, mtime, incremental behaviour,
  and that no `\Seen` flag was set on the server.
- `README.md` documents a restic recipe. `CHANGELOG.md` has the entry. No
  exported symbol changes.

## Hand-over to T29

T28 is complete. Under BOARD's lock-passing rule, `cmd/imapcli/auth.go` and
the test of its mechanism choice in `cmd/imapcli/backup_test.go` pass to
[T29](T29-list-extdata-auth.md). They worked around imapclient's automatic
SASL selection, which could send a password as a bearer token, and T29 adds
`AuthenticateOptions.CredentialKind` to make the workaround unnecessary.

Follow-up #21 takes `cmd/imapcli/auth.go` and its mechanism-choice test forward
from completed T29, whose spec records the same handover. It removes the copied
password-mechanism list in favour of the library's distinguishable local
automatic-selection refusal. No backup storage or synchronisation changes.
