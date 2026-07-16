#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Stops the QEMU VM and cleans up networking (bridge, TAP, FRR container).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BRIDGE_NAME="${QEMU_BRIDGE:-br-underlay}"
TAP_NAME="${QEMU_TAP:-vm-underlay}"
PID_FILE="${SCRIPT_DIR}/qemu.pid"
FRR_CONTAINER_NAME="${QEMU_FRR_CONTAINER:-qemu-tor}"

echo "Stopping QEMU VM..."

# Stop the VM.
if [[ -f "${PID_FILE}" ]]; then
    PID=$(cat "${PID_FILE}")
    if sudo kill -0 "${PID}" 2>/dev/null; then
        sudo kill "${PID}"
        echo "QEMU process ${PID} killed."
    else
        echo "QEMU process ${PID} not running."
    fi
    sudo rm -f "${PID_FILE}"
else
    echo "No PID file found, skipping VM stop."
fi

# Stop the FRR TOR container.
echo "Stopping FRR TOR container..."
if docker inspect "${FRR_CONTAINER_NAME}" &>/dev/null; then
    docker rm -f "${FRR_CONTAINER_NAME}"
    echo "FRR container ${FRR_CONTAINER_NAME} removed."
else
    echo "FRR container ${FRR_CONTAINER_NAME} not found, skipping."
fi

# Remove TAP device.
if ip link show "${TAP_NAME}" &>/dev/null; then
    sudo ip link del "${TAP_NAME}"
    echo "TAP device ${TAP_NAME} removed."
fi

# Remove bridge.
if ip link show "${BRIDGE_NAME}" &>/dev/null; then
    sudo ip link del "${BRIDGE_NAME}"
    echo "Bridge ${BRIDGE_NAME} removed."
fi

# Clean up generated files.
rm -f "${SCRIPT_DIR}/serial.log"
rm -f "${SCRIPT_DIR}/kubeconfig"

echo "Cleanup complete."
