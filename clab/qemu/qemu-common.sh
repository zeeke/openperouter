#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Common SSH helpers for QEMU VM scripts.
# Expects SCRIPT_DIR to be set by the sourcing script.

SSH_PORT="${QEMU_SSH_PORT:-2222}"
SSH_KEY="${SCRIPT_DIR}/qemu-vm-key"
chmod 600 "${SSH_KEY}" 2>/dev/null || true
SSH_CMD="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i ${SSH_KEY} -p ${SSH_PORT} openperouter@localhost"
SCP_CMD="scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i ${SSH_KEY} -P ${SSH_PORT}"

run_in_vm() {
    ${SSH_CMD} "sudo bash -c '$*'"
}
