#!/usr/bin/env bash
source "$(dirname "$0")/lib.sh"

ssh_cmd sudo poweroff || true
sleep 5
[ -f "$QEMU_WORKDIR/qemu.pid" ] && kill "$(cat "$QEMU_WORKDIR/qemu.pid")" 2>/dev/null || true
