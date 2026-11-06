#!/bin/sh
# Stops the worker before removal. An upgrade keeps it running.
set -e
case "$1" in
  remove|0)
    if [ -d /run/systemd/system ]; then
      systemctl disable --now deadfin-worker.service || true
    fi
    ;;
esac
