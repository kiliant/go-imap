#!/bin/sh
set -eu

password_hash=$(printf '%s\n%s\n' 'interop-pw' 'interop-pw' | userdbpw -md5)
printf 'interop@example.test\tuid=1000|gid=1000|home=/mail/interop|mail=/mail/interop/Maildir|systempw=%s\n' "$password_hash" >/etc/courier/userdb
chmod 0600 /etc/courier/userdb
makeuserdb

mkdir -p /run/courier/authdaemon
/etc/init.d/courier-authdaemon start

set +u
set -a
. /etc/courier/imapd
. /etc/courier/imapd-ssl
IMAP_STARTTLS=$IMAPDSTARTTLS
export IMAP_STARTTLS
set -u

exec /usr/sbin/couriertcpd -address=0 -maxprocs=40 -maxperip=40 -nodnslookup -noidentlookup 143 \
  /usr/lib/courier/courier/imaplogin /usr/bin/imapd Maildir
