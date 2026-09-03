#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Stops the QEMU VM, cleans up TAP devices and bridges, and destroys the
# clab topology.  Pass --destroy to also remove the VM disk image and SSH keys.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAB_NAME="${CLAB_NAME:-kind}"
NICS=(toswitch1 toswitch2 toleafkind1 toleafkind2)

DESTROY_IMAGE=false
if [[ "${1:-}" == "--destroy" ]]; then
    DESTROY_IMAGE=true
fi

# Stop QEMU VM
echo "[1/3] Stopping QEMU VM..."
if [[ -f "${SCRIPT_DIR}/qemu.pid" ]]; then
    PID=$(cat "${SCRIPT_DIR}/qemu.pid" 2>/dev/null || echo "")
    if [[ -n "${PID}" ]] && sudo kill -0 "${PID}" 2>/dev/null; then
        sudo kill "${PID}" 2>/dev/null || true
        sleep 2
        if sudo kill -0 "${PID}" 2>/dev/null; then
            sudo kill -9 "${PID}" 2>/dev/null || true
        fi
        echo "  QEMU stopped."
    else
        echo "  QEMU process not running."
    fi
    rm -f "${SCRIPT_DIR}/qemu.pid"
    rm -f "${SCRIPT_DIR}/monitor.sock"
else
    echo "  No PID file found, skipping."
fi


# Clean up TAP devices and bridges (one tap + bridge per NIC)
for nic in "${NICS[@]}"; do
    sudo ip tuntap del dev "${nic}_t" mode tap 2>/dev/null || true
    sudo ip link del "${nic}" 2>/dev/null || true
done

echo "  TAP devices cleaned up."

# Destroy containerlab topology
echo "[3/3] Destroying containerlab topology..."
if sudo containerlab inspect --name "${CLAB_NAME}" &>/dev/null 2>&1; then
    sudo containerlab destroy --name "${CLAB_NAME}" --cleanup
    echo "  Containerlab topology destroyed."
else
    # Clean up any orphaned clab containers
    for c in $(docker ps -aq --filter "name=clab-${CLAB_NAME}-" 2>/dev/null); do
        docker rm -f "$c" 2>/dev/null || true
    done
fi

# Remove orphaned bridges and veth pairs from clab links
for iface in leafkind1-sw leafkind2-sw pf0-up pf1-up; do
    sudo ip link del "${iface}" 2>/dev/null || true
done
echo "  Bridges and topology cleaned up."

# Optional: destroy VM image and keys
if [[ "${DESTROY_IMAGE}" == "true" ]]; then
    echo "Destroying VM image and SSH keys..."
    rm -f "${SCRIPT_DIR}/fedora-cloud.qcow2"
    rm -f "${SCRIPT_DIR}/cloud-init.iso"
    rm -f "${SCRIPT_DIR}/serial.log"
    rm -f "${SCRIPT_DIR}/monitor.sock"
    rm -f "${SCRIPT_DIR}/kubeconfig"
    rm -f "${SCRIPT_DIR}/qemu-vm-key"
    rm -f "${SCRIPT_DIR}/qemu-vm-key.pub"
    echo "  VM fully destroyed — run 'make qemu-image' to rebuild."
else
    echo "Cleanup complete. Disk image preserved — run 'make qemu-setup' to restart."
fi
