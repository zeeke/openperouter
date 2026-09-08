#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Shared paths and SSH helpers for scripts that manage the QEMU guest.

QEMU_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VM_DIR="${QEMU_DIR}/vm"
SSH_KEY="${VM_DIR}/qemu-vm-key"
QEMU_SSH_PORT="${QEMU_SSH_PORT:-2222}"
SSH_OPTIONS=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -i "${SSH_KEY}"
)

ssh_vm() {
    ssh "${SSH_OPTIONS[@]}" -p "${QEMU_SSH_PORT}" openperouter@127.0.0.1 "$@"
}

scp_to_vm() {
    local source_path=$1
    local destination_path=$2
    scp "${SSH_OPTIONS[@]}" -P "${QEMU_SSH_PORT}" \
        "${source_path}" "openperouter@127.0.0.1:${destination_path}"
}
