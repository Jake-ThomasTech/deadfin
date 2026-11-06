#!/bin/sh
# Sets the owner of the settings and state. The operator fills in
# /etc/deadfin/worker.env, then runs systemctl enable --now deadfin-worker.
set -e
chown root:deadfin-worker /etc/deadfin/worker.env
chown deadfin-worker:deadfin-worker /var/lib/deadfin-worker
if [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  # An upgrade restarts a running worker; the restart drains its jobs.
  systemctl try-restart deadfin-worker.service || true
fi
