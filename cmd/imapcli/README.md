# imapcli

`imapcli` is a read-only IMAP client for the terminal. It does two jobs:

- **Browse** an account: the folder tree, the newest messages in a folder, or
  one message by UID.
- **Back up** a whole account into a local directory in Maildir format. The
  backup is incremental and safe to re-run, and it is designed for restic or
  any other file-level backup tool to snapshot.

It never changes anything on the server. Mailboxes are opened with `EXAMINE`
and message bodies are read with `BODY.PEEK[]`, so not even the `\Seen` flag
is set. It sends no `STORE`, `APPEND`, `COPY`, `MOVE`, `CREATE`, `DELETE` or
`EXPUNGE`.

It is built only on the exported `imap` and `imapclient` packages of this
repository, like any other program that uses the library, and has no
dependencies beyond the Go standard library.

- [Install](#install)
- [Connecting](#connecting)
- [Passwords](#passwords)
- [Browsing](#browsing)
- [Backing up an account](#backing-up-an-account)
  - [Quick start](#quick-start)
  - [What is on disk](#what-is-on-disk)
  - [Incremental and safe to re-run](#incremental-and-safe-to-re-run)
  - [Safety checks](#safety-checks)
  - [Choosing mailboxes](#choosing-mailboxes)
  - [Output](#output)
  - [Long runs, timeouts and compression](#long-runs-timeouts-and-compression)
- [Scheduled backups with restic](#scheduled-backups-with-restic)
- [Restoring](#restoring)
- [Limitations](#limitations)
- [Reference](#reference)
- [Troubleshooting](#troubleshooting)
- [How it is tested](#how-it-is-tested)

## Install

```sh
go install github.com/kiliant/go-imap/cmd/imapcli@latest
```

This needs Go 1.24 or later and installs into `$(go env GOPATH)/bin`. From a
checkout, `go install ./cmd/imapcli` does the same. `imapcli`, backup mode
included, first shipped in v1.2.0 of the module.

`imapcli` runs on Linux, macOS and the BSDs. It also builds for other systems,
with two differences described under [Limitations](#limitations).

## Connecting

Every invocation needs a server, a user, and exactly one transport:

| Flag | Environment | Meaning |
|---|---|---|
| `-addr host:port` | `IMAP_ADDR` | Server address. Usually port 993 with `-tls`, 143 with `-starttls`. |
| `-user name` | `IMAP_USER` | Login name, often the full email address. |
| `-tls` | `IMAP_TLS=1` | Implicit TLS from the first byte. |
| `-starttls` | `IMAP_STARTTLS=1` | Connect in cleartext, then upgrade with STARTTLS before logging in. |
| `-plain` | — | Cleartext with no TLS at all, and the password is allowed over it. Only for local test servers. |
| `-insecure` | `IMAP_INSECURE=1` | Skip certificate verification. Only for test servers with self-signed certificates. |

A flag wins over its environment variable. The boolean variables accept `1`,
`true` or `yes`.

`imapcli` logs in only with mechanisms that take a password: SCRAM-SHA-256,
SCRAM-SHA-1 (each with its `-PLUS` channel-binding variant), CRAM-MD5, PLAIN
and LOGIN. The library picks the strongest one the server offers. If the
server advertises none of them, `imapcli` falls back to the LOGIN command,
unless the server announces `LOGINDISABLED`. A server that also offers OAuth
mechanisms, as Stalwart does by default, is never sent the password as a
token. Accounts that accept only OAuth cannot be used.

## Passwords

The password comes from exactly one of these sources:

| Source | Flag | Environment | Notes |
|---|---|---|---|
| Value | `-pass SECRET` | `IMAP_PASS` | Visible in the process list as a flag. Fine for a quick test, not for a job. |
| File | `-pass-file PATH` | `IMAP_PASS_FILE` | The first line of the file. A trailing CR or LF is removed and nothing else is trimmed. Keep the file at mode `600`. |
| Command | `-pass-cmd CMD` | `IMAP_PASS_CMD` | Runs `/bin/sh -c CMD` and uses the first line of its output. If the command fails, its stderr is part of the error. |
| Prompt | — | — | When none of the above is set: asks on the terminal with echo off. |

Giving two sources is a usage error, and an environment variable counts as a
source. So an exported `IMAP_PASS` plus `-pass-cmd` is refused with exit
status 2.

The prompt needs a terminal. It reads from standard input when that is a
terminal, and from `/dev/tty` otherwise. Under cron or launchd there is
neither, so an unattended job needs `-pass-file` or `-pass-cmd`.

A command keeps the password out of files, the process list and the job
definition. Some examples:

```sh
# macOS keychain. Store the password once; -w at the end makes it prompt:
security add-generic-password -s imapcli-mailbox.org -a me@mailbox.org -w
# ...and read it back on every run:
imapcli … -pass-cmd 'security find-generic-password -s imapcli-mailbox.org -w'

# pass (passwordstore.org); only the first line is used:
imapcli … -pass-cmd 'pass show mail/example.net'
```

## Browsing

All examples assume the connection settings are in the environment:

```sh
export IMAP_ADDR=imap.example.net:993 IMAP_TLS=1 IMAP_USER=alice@example.net
```

Progress messages (`imapcli: connecting to …`) go to standard error. Results
go to standard output, so `2>/dev/null` leaves only the results.

### The folder tree

With no mode flag, `imapcli` lists every mailbox and draws the hierarchy using
the server's own delimiter:

```console
$ imapcli
imapcli: connecting to imap.example.net:993...
imapcli: authenticating as alice@example.net...
imapcli: listing mailboxes...
├── Archive
│   └── 2024
├── INBOX
├── Junk
└── Sent
```

Mailboxes that cannot be opened are marked `[noselect]` or `[nonexistent]`.
When the server announces special-use folders, they are marked with their
role, as in `Sent  [\Sent]`. The roles are `\Sent`, `\Drafts`, `\Junk`,
`\Trash`, `\Archive`, `\All`, `\Flagged` and `\Important`. `-subscribed`
lists only subscribed mailboxes.

### Messages in a mailbox

`-mailbox NAME` opens one mailbox read-only and lists the newest 50 messages:

```console
$ imapcli -mailbox INBOX 2>/dev/null
Mailbox: INBOX (read-only)
Messages: 3  Recent: 0  First unseen: seq 3
UIDNEXT: 4  UIDVALIDITY: 1

   UID  DATE              FROM                          SUBJECT
     1  2026-10-12 09:14  Bob                           Lunch on Friday? [\Seen]
     2  2026-10-13 17:02  Carol                         Invoice 2026-114 [\Seen \Flagged $Forwarded]
     3  2026-10-14 08:30  Dave                          Release notes draft
```

- `-limit N` changes how many messages are listed. `-limit 0` lists all of
  them.
- `-offset N` skips the N newest first, so `-limit 20 -offset 20` shows
  messages 21 to 40 counted from the newest.
- `First unseen` comes from the server's UNSEEN response code. It is the
  sequence number of the first unseen message: its position in the mailbox,
  counting the oldest as 1. It is not a UID and not a count. Above, the third
  message is the first unseen one. The field is left out when the server does
  not report it.
- The date is the message's `Date:` header in local time, and the sender is
  the display name, or the address when there is no name.
- Flags and keywords follow the subject in brackets.

### One message

`-mailbox NAME -uid N` prints the mailbox header and the message's summary
line, then the complete raw message as the server stores it, headers and all:

```sh
imapcli -mailbox INBOX -uid 2 2>/dev/null | less
```

The UID is the first column of the listing. Because the message is read with
`BODY.PEEK[]`, printing it does not mark it as read.

Browsing stops after `-timeout` (default two minutes); `-timeout 0` waits
indefinitely.

## Backing up an account

`-backup DIR` turns `imapcli` into a mirror. It keeps `DIR` an exact copy of
every mailbox in the account, in Maildir format: one file per message. The
first run downloads everything. Each later run downloads only what is new,
renames the files whose flags changed, and deletes what the server deleted.

A mirror on its own is not a history: it forgets what the server forgets. The
history comes from snapshotting `DIR` with a backup tool such as restic. The
layout is designed for that. A message file is written once and never
rewritten, so the tool stores each message once, no matter how many snapshots
contain it.

### Quick start

Choose a persistent directory, such as `~/MailBackup/mailbox.org`. Do not use
`/tmp`, macOS `$TMPDIR`, or another automatically cleaned location: the
operating system may remove files between runs. A message's file modification
time records its IMAP INTERNALDATE, which can be years old; it does not say
when the file was downloaded or protect it from cleanup.

If local message files disappear, the next run downloads them again only if
they still exist on the server. Keep the mirror on persistent storage and
snapshot it with restic or another backup tool to retain deleted messages.

Run the first backup by hand, so you can watch it and check the result:

```sh
imapcli -addr imap.mailbox.org:993 -tls -user me@mailbox.org \
    -pass-cmd 'security find-generic-password -s imapcli-mailbox.org -w' \
    -backup ~/MailBackup/mailbox.org
echo "exit status $?"
```

It prints one line per mailbox and a summary:

```text
imapcli: connecting to imap.mailbox.org:993...
imapcli: authenticating as me@mailbox.org...
imapcli: listing mailboxes...
imapcli: backing up Archive...
Archive: 0 messages, 0 new, 0 flags changed, 0 removed
imapcli: backing up Archive/2024...
Archive/2024: 1 messages, 1 new, 0 flags changed, 0 removed
imapcli: backing up INBOX...
INBOX: 3 messages, 3 new, 0 flags changed, 0 removed
imapcli: backing up Junk...
Junk: 1 messages, 1 new, 0 flags changed, 0 removed
imapcli: backing up Sent...
Sent: 1 messages, 1 new, 0 flags changed, 0 removed
backup: 5 mailboxes, 6 messages, 6 new, 0 flags changed, 0 removed, 0 mailboxes pruned, 0 failed
```

Exit status 0 means every mailbox was backed up. Run the same command again:
with nothing changed on the server, the summary reports `0 new, 0 flags
changed, 0 removed` and no message file is touched. Then hand it to a
scheduler: see [Scheduled backups with restic](#scheduled-backups-with-restic).

A first backup of a large account can take hours. It does not have to finish
in one go: see [Interrupted runs](#interrupted-runs).

### What is on disk

```text
~/MailBackup/mailbox.org/
├── .imapcli-backup.json          which account this directory belongs to
├── .imapcli-backup.lock          held while a backup runs
├── Archive/
│   ├── .imapcli-mailbox.json     mailbox name, UIDVALIDITY, keywords
│   ├── cur/  new/  tmp/
│   └── 2024/                     the child mailbox Archive/2024
│       ├── .imapcli-mailbox.json
│       ├── cur/
│       │   └── V3.U1:2,RS
│       └── new/  tmp/
├── INBOX/
│   ├── .imapcli-mailbox.json
│   ├── cur/
│   │   ├── V1.U1:2,S
│   │   ├── V1.U2:2,FS
│   │   └── V1.U3:2,
│   └── new/  tmp/
├── Junk/ …
└── Sent/ …
```

**One directory per mailbox, nested like the server's hierarchy.** Names are
split on the server's own delimiter, so `Archive/2024` on a server that uses
`/` and `Archive.2024` on one that uses `.` both become `Archive/2024/`. A
mailbox that is a parent and also holds messages, like `Archive` above, has
its own `cur/`, `new/` and `tmp/` next to its children's directories. A
`\Noselect` mailbox holds no messages and gets no Maildir of its own.

Non-ASCII names are stored as UTF-8. A few characters cannot appear in a path
segment as they are, so they are percent-escaped:

| Name segment | Directory | Why |
|---|---|---|
| `100%` | `100%25` | `%` is the escape character itself |
| `a/b` (on a server whose delimiter is `.`) | `a%2Fb` | `/` would split the path |
| control characters | `%00` to `%1F`, `%7F` | not safe in a file name |
| `.hidden`, `.`, `..` | `%2Ehidden`, `%2E`, `%2E.` | hidden or special in a file system |
| `cur`, `new`, `tmp` | `%63ur`, `%6Eew`, `%74mp` | would collide with the Maildir's own directories |
| empty (from a doubled delimiter) | `%` | a directory needs a name |

**One file per message.** Each file is named
`V<uidvalidity>.U<uid>:2,<flags>` and lives in `cur/`:

- The content is exactly what the server returns for `BODY[]`, byte for byte,
  CRLF line endings included. It is a complete RFC 5322 message: renamed to
  `.eml`, any mail program opens it.
- The file's modification time is the message's INTERNALDATE, the time the
  server received it. Maildir readers show that as the received date, and
  restic preserves it.
- `<flags>` holds the standard Maildir info letters, in this order:

  | Letter | IMAP flag |
  |---|---|
  | `D` | `\Draft` |
  | `F` | `\Flagged` |
  | `R` | `\Answered` |
  | `S` | `\Seen` |
  | `T` | `\Deleted` |

  `V1.U3:2,` is an unread message with no flags. `V1.U2:2,FS` is flagged and
  read.

**Everything else about a mailbox** is in its `.imapcli-mailbox.json`: the
exact server-side name, the UIDVALIDITY, and every flag a file name cannot
carry, keywords such as `$Forwarded` included, listed by UID. `\Recent` is a
per-session flag and is not kept.

```json
{
  "version": 1,
  "mailbox": "INBOX",
  "uidvalidity": 1,
  "keywords": {
    "2": [
      "$Forwarded"
    ]
  }
}
```

**`.imapcli-backup.json`** records the account the directory belongs to: the
host name from `-addr`, lower-cased and without the port, and the user name.

```json
{
  "version": 1,
  "host": "imap.mailbox.org",
  "user": "me@mailbox.org"
}
```

### Incremental and safe to re-run

A backup run does not replay a change log, and it keeps no cursor that could
fall out of step with the files. For each mailbox it compares two lists, what
the server has now and what is on disk now, and changes the disk until the two
match:

1. `EXAMINE` the mailbox and read its UIDVALIDITY.
2. Ask the server for the UID and flags of every message
   (`UID FETCH 1:* (UID FLAGS)`). This list is the authority for the run.
3. List the message files on disk. The UID and UIDVALIDITY are in each file
   name.
4. Rename every file whose flags differ from the server's.
5. Download every message the server has and the disk lacks, in batches of
   200, with `BODY.PEEK[]`.
6. Only after all downloads succeeded: delete the files of messages that are
   no longer on the server.
7. Rewrite `.imapcli-mailbox.json` and flush the directories to disk.

After every mailbox has succeeded, local copies of mailboxes that no longer
exist on the server are removed.

The end state depends only on what the server holds when the run starts, not
on the history of earlier runs. That has practical consequences:

- **Running it again changes nothing.** If the server has not changed, a run
  downloads nothing, renames nothing and deletes nothing. Message files that
  are already correct are not opened for writing, so their content,
  modification time and inode stay the same. Only each mailbox's small state
  file is rewritten, with the same content.
- **Missed runs do not matter.** A run after a week away catches up with a
  week of changes in one go. How often it runs only decides how fresh the
  copy is.
- **The backup repairs itself.** A message file deleted from `DIR` by mistake
  is downloaded again by the next run. A file renamed by a mail program, for
  example when you open the backup in mutt and it marks a message as read, is
  renamed back to the server's flags.

What each kind of change on the server costs the next run, and costs restic:

| Change on the server | Next run | New data in restic |
|---|---|---|
| New message | Downloaded once into `cur/` | The message |
| Flags changed | File renamed: same content, same mtime | None |
| Keyword added or removed | Recorded in `.imapcli-mailbox.json` | A few bytes |
| Message expunged | File deleted | None; older snapshots still have it |
| Message moved to another folder | Downloaded into the new folder, deleted from the old one | None: identical content is deduplicated |
| Folder created | New Maildir, messages downloaded | Its messages |
| Folder renamed | Downloaded under the new name, old copy removed | None: identical content is deduplicated |
| Folder deleted | Local copy removed | None |
| UIDVALIDITY changed (mailbox rebuilt or recreated) | Every message downloaded again under the new value, then the old files removed | None for messages whose content is unchanged |

#### Interrupted runs

A run can be stopped at any point: Ctrl-C, a crash, a dropped connection, a
laptop going to sleep, a power cut. The next run continues where it stopped.

- A message is never written in place. Its body is streamed into `tmp/`,
  flushed to disk, given its modification time, and only then renamed into
  `cur/`. Every file in `cur/` is therefore complete. A backup tool that
  snapshots during a run, or after an interrupted one, never stores half a
  message.
- `.imapcli-mailbox.json` and `.imapcli-backup.json` are replaced atomically,
  through a temporary file and a rename.
- Partial files left in `tmp/` are removed by the next run.
- The next run downloads only what is still missing. A first backup of a very
  large account can be spread across as many runs as it takes.

#### When a mailbox fails

If one mailbox fails, for example because the server refuses to open it, the
run reports it and continues with the others. The failed mailbox keeps the
copy from its last successful run. In a run with any failure, no mailbox is
pruned: a mailbox missing from the server's list might be a server error, not
a deletion. The exit status is 1, so a scheduler or wrapper script notices.

Within one mailbox, deletions come last. If a download fails, nothing is
deleted from that mailbox in that run.

### Safety checks

Before it connects, `imapcli` checks the directory it is about to mirror into:

- **One account per directory.** The first run writes
  `.imapcli-backup.json`. A later run for a different user or host is refused,
  so two accounts can never be mixed in, or pruned from, one tree. The host is
  compared without its port and ignoring case; the user is compared exactly.
- **Only new, empty or earlier backup directories.** A non-empty directory
  without `.imapcli-backup.json` is refused. A mistyped `-backup ~/Documents`
  therefore cannot fill or prune anything.
- **One run at a time.** A run holds an exclusive lock on
  `.imapcli-backup.lock` until it exits. A second run on the same directory
  fails immediately, before it asks for a password. On Linux, macOS and the
  BSDs this is a `flock`, which the kernel releases if the process dies, so a
  crash never leaves a stale lock.
- **Only its own files.** Pruning deletes only message files, temporary files,
  Maildir directories and state files that `imapcli` itself wrote. Other files
  in the tree are left alone, and so are the directories that still hold them.
- **No silent merges.** On a case-insensitive file system, such as APFS in its
  default setting, two mailboxes named `Work` and `work` map to the same
  directory. The state file records the exact mailbox name, so the second one
  fails with an error instead of being merged into the first. To back up such
  an account, exclude one of the two, or put `DIR` on a case-sensitive volume.

### Choosing mailboxes

Every mailbox the server lists is backed up, except those marked `\Noselect`
or `\NonExistent`. `-exclude PATTERN` skips mailboxes by full name, and can be
given more than once:

```sh
imapcli … -backup ~/MailBackup/example.net -exclude Junk -exclude 'Trash' -exclude 'Trash/*'
```

- `*` matches any run of characters, the hierarchy delimiter included. `?`
  matches exactly one character. Nothing else is special, and matching is
  case-sensitive.
- The pattern must match the whole name. `Trash*` matches `Trash`,
  `Trash/Old` and also `Trashcan`.
- Patterns use the server's names and delimiter. On a server that puts every
  folder under `INBOX.`, the junk folder is `INBOX.Junk`. Run `imapcli`
  without a mode flag to see the names.
- Quote patterns so the shell does not expand them.

An excluded mailbox is not part of the backup. **If an earlier run had backed
it up, its local copy is removed.** Older restic snapshots still contain it.

### Output

| Stream | Content |
|---|---|
| Standard output | One line per mailbox backed up, then the summary line |
| Standard error | Progress, skipped mailboxes, warnings and errors |

The numbers in a mailbox line are: messages on the server, messages
downloaded, files renamed for a flag change, and files removed. The summary
adds up all mailboxes and also counts pruned mailbox copies and failures.

Two warnings do not fail a run:

- `N of M requested messages were not returned (expunged meanwhile?)`: messages
  disappeared between the listing and the download. If they still exist, the
  next run fetches them.
- `server reported RFC822.SIZE X, BODY[] had Y octets`: the server's size
  disagrees with the bytes it sent. The bytes are stored as sent.

| Exit status | Meaning |
|---|---|
| 0 | Every mailbox was backed up. |
| 1 | Something failed: the connection, the login, the directory checks, or one or more mailboxes. The failures are on standard error. The mailboxes that worked are still backed up. |
| 2 | Usage error, such as a missing or conflicting flag, or no password could be obtained: an unreadable file, a failing command, or no terminal to prompt on. Nothing was sent to the server. |

### Long runs, timeouts and compression

- `-timeout` is a limit on the whole run. In backup mode it has no default,
  because a first backup can take hours. Set it only if you want a hard cap.
- A connection that stops delivering data fails after five minutes of
  silence, so a dead connection cannot hang a scheduled job forever.
- COMPRESS=DEFLATE is used when the server offers it. If it fails, the run
  continues uncompressed. `-compress=false` turns it off.
- One connection handles the whole account, one mailbox after another.

## Scheduled backups with restic

The pattern is: mirror each account into its own directory, then snapshot the
parent directory with restic. Restic provides the history, the deduplication
and the encryption.

**1. Store the passwords** in the keychain, or in files only you can read:

```sh
security add-generic-password -s imapcli-mailbox.org -a me@mailbox.org -w
security add-generic-password -s imapcli-example.net -a me@example.net -w
```

**2. Run each account once by hand**, as in the [Quick start](#quick-start),
and check that a second run reports `0 new`.

**3. Write the job script**, for example `~/bin/mail-backup.sh`. Use absolute
paths: launchd and cron start with a minimal `PATH`.

```sh
#!/bin/sh
# Mirror two IMAP accounts, then snapshot the mirrors with restic.
set -u
IMAPCLI="$HOME/go/bin/imapcli"
RESTIC=/opt/homebrew/bin/restic
export RESTIC_REPOSITORY="$HOME/Backups/restic-mail"   # or sftp:…, s3:…, rest:…
export RESTIC_PASSWORD_COMMAND='security find-generic-password -s restic-mail -w'

status=0
"$IMAPCLI" -addr imap.mailbox.org:993 -tls -user me@mailbox.org \
    -pass-cmd 'security find-generic-password -s imapcli-mailbox.org -w' \
    -backup "$HOME/MailBackup/mailbox.org" -exclude Junk || status=1
"$IMAPCLI" -addr mail.example.net:993 -tls -user me@example.net \
    -pass-cmd 'security find-generic-password -s imapcli-example.net -w' \
    -backup "$HOME/MailBackup/example.net" || status=1

# Snapshot even after a failure: a failed mailbox keeps its previous copy, and
# nothing is pruned in a run that had one.
"$RESTIC" backup --tag mail "$HOME/MailBackup" || status=1
"$RESTIC" forget --tag mail --keep-daily 7 --keep-weekly 5 --keep-monthly 12 --prune || status=1
exit $status
```

The restic repository must exist first (`restic init`). The `forget` policy is
an example; restic's documentation describes the options.

**4a. Schedule it with launchd on macOS.** Save this as
`~/Library/LaunchAgents/local.mail-backup.plist`, with your home directory in
the paths:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>local.mail-backup</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>/Users/me/bin/mail-backup.sh</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key>
    <integer>3</integer>
    <key>Minute</key>
    <integer>15</integer>
  </dict>
  <key>StandardOutPath</key>
  <string>/Users/me/Library/Logs/mail-backup.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/me/Library/Logs/mail-backup.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/local.mail-backup.plist
launchctl kickstart gui/$(id -u)/local.mail-backup     # run it once now
```

- A LaunchAgent runs in your login session, which is what lets it read the
  login keychain. If macOS asks whether `security` may use a keychain item,
  choose *Always Allow*, or the unattended run will wait on the dialog.
- If the Mac is asleep at 03:15, launchd starts the job when it wakes. Since
  runs are incremental, a late run simply has more to catch up on.
- Keep `~/MailBackup` and the script outside `~/Desktop`, `~/Documents`,
  `~/Downloads` and iCloud Drive. macOS restricts background access to those
  folders.

**4b. Or schedule it with cron:**

```text
15 3 * * * /home/me/bin/mail-backup.sh >>/home/me/.local/state/mail-backup.log 2>&1
```

Use `-pass-file` or a command that works without a desktop session, such as
`pass` with a gpg-agent the job can reach. Overlapping `imapcli` runs are
harmless: a second one on the same directory exits at once with status 1.

**What a snapshot contains.** Each snapshot holds the mirror as the last run
left it, with every message file whole. The mailboxes are copied one after
another, so they reflect the server at slightly different moments. The state
files are small and their content changes only with the mail, so the cost of
a daily snapshot is roughly the new mail since the previous one.

## Restoring

`imapcli` never writes to a server, so restoring is done with standard Maildir
tools. Get the files back first, into a scratch directory rather than over
the live mirror:

```sh
restic restore latest --tag mail --target ~/mail-restore
# or browse every snapshot as a file system (needs FUSE):
restic mount ~/mnt/restic
```

Then, depending on what you need:

- **One message:** copy its file to `message.eml` and open it in any mail
  program.
- **Read a folder:** `mutt -R -f ~/mail-restore/…/MailBackup/mailbox.org/INBOX`
  opens it read-only. notmuch, neomutt and other Maildir readers work too.
- **Find a message:** each file is plain text, so
  `grep -rl 'Message-ID: <…>' ~/mail-restore` finds it.
- **Upload folders back to a server:** use a tool that syncs a Maildir to
  IMAP, such as mbsync (isync). Its Maildir store with `SubFolders Verbatim`
  matches the nested layout, and `Inbox` points at the `INBOX` directory. Run
  it on the restored copy, not on the live mirror: such tools add their own
  state files and rename message files.

Maildir tools understand only the five standard flags in the file name.
Keywords such as `$Forwarded` are in `.imapcli-mailbox.json`, listed by the UID
in each file name, and have to be set again by hand if they matter.

## Limitations

- **No restore command.** Uploading would end the promise that `imapcli`
  never changes the server. Use the Maildir tools above.
- **No OAuth.** Only password logins are supported.
- **Every run lists every message.** A run asks for the UID and flags of every
  message, even when nothing changed: a few dozen bytes per message, a few
  megabytes for 100 000 messages. That listing is also how deletions are
  found. Bodies are only downloaded once. CONDSTORE/QRESYNC-based change
  tracking is not used.
- **Stalwart and brand-new folders.** In Stalwart 0.11.8, a folder another
  session has just created and filled can stay invisible to other sessions
  while that session is still open. Such a folder is backed up one run late.
  A folder already in the backup is never removed because of this.
- **Other operating systems.** Outside Linux, macOS and the BSDs, there is no
  password prompt, and the lock is a plain file. After a crash, that file has
  to be removed by hand; the error message says so.

## Reference

| Flag | Default | Environment | Description |
|---|---|---|---|
| `-addr` | — | `IMAP_ADDR` | Server address, `host:port`. Required. |
| `-user` | — | `IMAP_USER` | Login name. Required. |
| `-tls` | off | `IMAP_TLS` | Implicit TLS. |
| `-starttls` | off | `IMAP_STARTTLS` | STARTTLS upgrade. |
| `-plain` | off | — | Cleartext, password allowed over it. Test servers only. |
| `-insecure` | off | `IMAP_INSECURE` | Skip TLS certificate verification. Test servers only. |
| `-pass` | — | `IMAP_PASS` | Password as a value. |
| `-pass-file` | — | `IMAP_PASS_FILE` | Password from the first line of a file. |
| `-pass-cmd` | — | `IMAP_PASS_CMD` | Password from the first line a `/bin/sh` command prints. |
| `-subscribed` | off | — | Folder tree: subscribed mailboxes only. |
| `-mailbox` | — | — | List messages in this mailbox. |
| `-limit` | 50 | — | With `-mailbox`: how many of the newest messages. `0` lists all. |
| `-offset` | 0 | — | With `-mailbox`: skip this many newest messages first. |
| `-uid` | — | — | With `-mailbox`: print this message. |
| `-backup` | — | — | Mirror the account into this directory. |
| `-exclude` | — | — | With `-backup`: skip mailboxes matching this pattern. Repeatable. |
| `-compress` | on | — | With `-backup`: use COMPRESS=DEFLATE when offered. |
| `-timeout` | 2m; none with `-backup` | — | Limit on the whole run. `0` means none. |

Exactly one of `-tls`, `-starttls` and `-plain` is required. At most one
password source may be set. `-backup` cannot be combined with `-mailbox`,
`-uid` or `-subscribed`, and `-exclude` needs `-backup`.

`imapcli -h` prints the same list.

## Troubleshooting

| Message | Cause and fix |
|---|---|
| `another imapcli backup is running on this directory` | Another run holds the lock. Wait for it to finish. On systems without `flock`, a crashed run leaves the lock file behind; remove it if no backup is running. |
| `… holds the backup of A on H, not B on H` | `-backup` points at another account's directory. Check the path. If the account itself moved to a new host name, edit `host` in `.imapcli-backup.json`. |
| `… is not empty and is not an imapcli backup` | `-backup` points at an existing directory with other content. Use a new or empty one. |
| `… already holds mailbox "x"; the file system does not tell the two names apart` | Two mailboxes differ only in case or Unicode normalisation, and the file system treats them as one name. Exclude one, or use a case-sensitive volume. If a folder was only renamed in case, for example `work` to `Work`, delete the old local directory (restic keeps it) and run again. |
| `password command: exit status 1: …` | The `-pass-cmd` command failed; its own error follows. Run it by hand to see why. |
| `auth: imap: NO … [AUTHENTICATIONFAILED]` | Wrong user or password. Some providers require an app-specific password for IMAP. |
| `use only one of -pass, -pass-file and -pass-cmd …` | Two password sources are set. An exported `IMAP_PASS`, `IMAP_PASS_FILE` or `IMAP_PASS_CMD` counts. |
| `terminal: … (set -pass or IMAP_PASS for non-interactive use)` or `open /dev/tty: …` | No terminal to prompt on, as under cron or launchd. Use `-pass-file` or `-pass-cmd`. |
| `exactly one of -tls, -starttls, or -plain is required` | Choose the transport: `-tls` for port 993, `-starttls` for 143. |

## How it is tested

- **Unit tests** (`go test ./cmd/imapcli`) run the real command against a
  scripted fake server: a first run, an incremental run with a new message, a
  flag change, an expunge, a deleted mailbox and a UIDVALIDITY change, then a
  no-op run. They check that an unchanged file keeps its inode, that a server
  sending UID after the body is handled, and that failures, name collisions,
  foreign files, account markers, locks and password sources behave as
  described here. The fake server records every command outside a read-only
  allowlist, and the tests fail if the backup sends one.
- **Interop tests** (`go test -tags=interop ./interop/imapcli`, needs podman)
  build the binary and run it three times against live Dovecot and Stalwart
  servers: first, after changes, and with nothing to do. They compare every
  file byte for byte with the server's copy, check flags, modification times,
  nested and non-ASCII mailbox names, and check that the backup changed no
  flag on the server.
