#!/bin/sh
# Creates the system user of the worker unit.
set -e
if ! getent group deadfin-worker >/dev/null; then
  groupadd --system deadfin-worker
fi
if ! getent passwd deadfin-worker >/dev/null; then
  useradd --system --gid deadfin-worker --home-dir /var/lib/deadfin-worker --no-create-home --shell /usr/sbin/nologin deadfin-worker
fi
for group in render video; do
  if getent group "$group" >/dev/null; then
    usermod --append --groups "$group" deadfin-worker
  fi
done
