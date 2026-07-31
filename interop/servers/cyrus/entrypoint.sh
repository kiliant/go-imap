#!/bin/sh
set -eu

printf '%s' 'interop-admin-pw' | saslpasswd2 -p -c -u example.test cyrus
printf '%s' 'interop-pw' | saslpasswd2 -p -c -u example.test interop
chown cyrus:mail /etc/sasldb2

/usr/sbin/cyrmaster -C /etc/imapd.conf -M /etc/cyrus.conf &
master_pid=$!

i=0
until printf 'cm user/interop@example.test\n' | cyradm --auth plain --user cyrus@example.test --password interop-admin-pw localhost >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 100 ]; then
    echo 'Cyrus account provisioning timed out' >&2
    kill "$master_pid"
    wait "$master_pid"
    exit 1
  fi
done

wait "$master_pid"
