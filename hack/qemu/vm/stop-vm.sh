#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Stops the QEMU VM and cleans up networking (bridge, TAP, FRR container).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BRIDGE_NAME="${QEMU_BRIDGE:-br-underlay}"
TAP_PREFIX="${QEMU_TAP_PREFIX:-vm-underlay}"
NUM_NICS=4
PID_FILE="${SCRIPT_DIR}/qemu.pid"
VM_IMAGE="${SCRIPT_DIR}/fedora-cloud.qcow2"
FRR_CONTAINER_NAME="${QEMU_FRR_CONTAINER:-qemu-tor}"

echo "Stopping QEMU VM..."

# Stop the VM.
if [[ -f "${PID_FILE}" ]]; then
    PID=$(cat "${PID_FILE}" 2>/dev/null || sudo cat "${PID_FILE}")
    if sudo kill -0 "${PID}" 2>/dev/null; then
        sudo kill "${PID}"
        echo "QEMU process ${PID} killed."
    else
        echo "QEMU process ${PID} not running."
    fi
    rm -f "${PID_FILE}" 2>/dev/null || sudo rm -f "${PID_FILE}"
else
    # Try to find and kill QEMU by process name as fallback.
    QEMU_PID=$(pgrep -f "qemu-system-x86_64.*${VM_IMAGE##*/}" 2>/dev/null || true)
    if [[ -n "${QEMU_PID}" ]]; then
        sudo kill "${QEMU_PID}"
        echo "QEMU process ${QEMU_PID} killed (found by process name)."
    else
        echo "No PID file found and no QEMU process detected, skipping VM stop."
    fi
fi

# Stop the FRR TOR container.
echo "Stopping FRR TOR container..."
if docker inspect "${FRR_CONTAINER_NAME}" &>/dev/null; then
    docker rm -f "${FRR_CONTAINER_NAME}"
    echo "FRR container ${FRR_CONTAINER_NAME} removed."
else
    echo "FRR container ${FRR_CONTAINER_NAME} not found, skipping."
fi

# Remove TAP devices.
for i in $(seq 0 $((NUM_NICS - 1))); do
    TAP="${TAP_PREFIX}-${i}"
    if ip link show "${TAP}" &>/dev/null; then
        sudo ip link del "${TAP}"
        echo "TAP device ${TAP} removed."
    fi
done

# Remove bridge.
if ip link show "${BRIDGE_NAME}" &>/dev/null; then
    sudo ip link del "${BRIDGE_NAME}"
    echo "Bridge ${BRIDGE_NAME} removed."
fi

# Clean up generated files.
rm -f "${SCRIPT_DIR}/serial.log"
rm -f "${SCRIPT_DIR}/kubeconfig"

# If --destroy was passed, remove the disk image and cloud-init ISO
# so the next 'make qemu-image' starts from scratch.
if [[ "${1:-}" == "--destroy" ]]; then
    echo "Destroying VM disk image and cloud-init ISO..."
    rm -f "${VM_IMAGE}"
    rm -f "${SCRIPT_DIR}/cloud-init.iso"
    rm -f "${SCRIPT_DIR}/qemu-ssh-key" "${SCRIPT_DIR}/qemu-ssh-key.pub"
    echo "VM fully destroyed — run 'make qemu-image' to rebuild."
else
    echo "Cleanup complete. Disk image preserved — rerun 'make qemu-setup' to restart the same VM."
    echo "To fully destroy, run 'make qemu-destroy'."
fi
