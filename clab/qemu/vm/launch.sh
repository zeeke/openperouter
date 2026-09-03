#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Launches QEMU VM with 4 igb NICs.
# Each NIC is backed by a TAP on the host, plugged into a Linux bridge.
#
# Naming convention (e.g. for toswitch1):
#   Host:  toswitch1 (bridge) --- toswitch1_t (tap) | toswitch1 (igb) Guest
#
# The guest NIC name matches the bridge name via udev MAC-based rules.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

VM_IMAGE="${SCRIPT_DIR}/fedora-cloud.qcow2"
CLOUD_INIT_ISO="${SCRIPT_DIR}/cloud-init.iso"
CLAB_NAME="${CLAB_NAME:-kind}"
NICS=(toswitch1 toswitch2 toleafkind1 toleafkind2)
SSH_PORT="${QEMU_SSH_PORT:-2222}"
K8S_PORT="${QEMU_K8S_PORT:-6443}"
VM_CPUS="${QEMU_VM_CPUS:-4}"
VM_MEM="${QEMU_VM_MEM:-6144}"
PID_FILE="${SCRIPT_DIR}/qemu.pid"
SERIAL_LOG="${SCRIPT_DIR}/serial.log"

# Avoid absolute path, as they can cause the error:
# qemu-system-x86_64: -monitor unix:.../monitor.sock,server,nowait: UNIX socket path '.../monitor.sock' is too long
#Path must be less than 108 bytes
MONITOR_SOCK="/tmp/monitor.sock"
SSH_KEY="${SCRIPT_DIR}/qemu-vm-key"
chmod 600 "${SSH_KEY}"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 -i ${SSH_KEY}"
MAX_WAIT=300

wait_for_ssh() {
    local reason=$1
    local elapsed=0
    while ! ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost true 2>/dev/null; do
        sleep 5
        elapsed=$((elapsed + 5))
        if [[ "${elapsed}" -ge "${MAX_WAIT}" ]]; then
            echo "ERROR: VM did not become SSH-reachable ${reason} within ${MAX_WAIT}s." >&2
            tail -50 "${SERIAL_LOG}" >&2 || true
            exit 1
        fi
        echo "  still waiting... (${elapsed}s / ${MAX_WAIT}s)"
    done
}

# Reboot once if cloud-init's grubby args (intel_iommu=on, hugepages) are not
# yet on the running kernel. vfio-pci cannot bind without a guest IOMMU.
ensure_iommu_cmdline() {
    if ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
        "grep -q intel_iommu=on /proc/cmdline" 2>/dev/null; then
        return 0
    fi

    echo "Rebooting VM for kernel cmdline changes (IOMMU, hugepages)..."
    ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
        "sudo reboot" 2>/dev/null || true
    sleep 10
    echo "Waiting for VM to become SSH-reachable after reboot..."
    wait_for_ssh "after reboot"
    echo "VM is back after reboot."
}

# Skip QEMU launch if the VM is already running, but still apply the IOMMU
# cmdline reboot if this boot happened before grubby took effect.
if [[ -f "${PID_FILE}" ]]; then
    PID=$(cat "${PID_FILE}" 2>/dev/null || echo "")
    if [[ -n "${PID}" ]] && sudo kill -0 "${PID}" 2>/dev/null; then
        echo "QEMU VM is already running (PID ${PID}), skipping launch."
        wait_for_ssh "on already-running VM"
        ensure_iommu_cmdline
        echo "VM is ready."
        exit 0
    fi
fi

if [[ ! -f "${VM_IMAGE}" ]]; then
    echo "ERROR: VM image not found at ${VM_IMAGE}. Run 'make qemu-image' first." >&2
    exit 1
fi

if [[ ! -f "${CLOUD_INIT_ISO}" ]]; then
    echo "ERROR: cloud-init ISO not found at ${CLOUD_INIT_ISO}. Run 'make qemu-image' first." >&2
    exit 1
fi

# --- Networking setup ---
# Create a bridge + TAP pair for each NIC. The bridge name matches the guest
# interface name; the TAP has a _t suffix.  See ARCHITECTURE.md for the diagram.
echo "Setting up TAP devices and connecting to bridges..."

NIC_ARGS=""
slot=1

for nic in "${NICS[@]}"; do
    tap="${nic}_t"
    mac=$(printf "52:54:00:ab:cd:%02x" "${slot}")

    if [[ ! -d "/sys/class/net/${nic}" ]]; then
        sudo ip link add name "${nic}" type bridge
    fi
    sudo ip link set dev "${nic}" up

    sudo ip tuntap add dev "${tap}" mode tap
    sudo ip link set "${tap}" master "${nic}"
    sudo ip link set "${tap}" up

    NIC_ARGS="${NIC_ARGS} -device pcie-root-port,id=rp${slot},slot=${slot}"
    NIC_ARGS="${NIC_ARGS} -netdev tap,id=${tap},ifname=${tap},script=no,downscript=no"
    NIC_ARGS="${NIC_ARGS} -device igb,bus=rp${slot},netdev=${tap},mac=${mac}"

    slot=$((slot + 1))
done

# --- Launch QEMU ---
echo "Launching QEMU VM with ${#NICS[@]} igb NICs..."

touch "${SERIAL_LOG}" "${PID_FILE}"

sudo qemu-system-x86_64 \
    -machine q35,kernel-irqchip=split \
    -device intel-iommu,intremap=on,caching-mode=on \
    -enable-kvm \
    -cpu host \
    -smp "${VM_CPUS}" \
    -m "${VM_MEM}" \
    -drive file="${VM_IMAGE}",if=virtio,format=qcow2 \
    -cdrom "${CLOUD_INIT_ISO}" \
    -netdev user,id=mgmt,hostfwd=tcp::${SSH_PORT}-:22,hostfwd=tcp::${K8S_PORT}-:6443 \
    -device virtio-net-pci,netdev=mgmt \
    ${NIC_ARGS} \
    -display none \
    -serial file:"${SERIAL_LOG}" \
    -monitor unix:"${MONITOR_SOCK}",server,nowait \
    -pidfile "${PID_FILE}" \
    -daemonize

sudo chown "$(id -u):$(id -g)" "${SERIAL_LOG}" "${PID_FILE}" 2>/dev/null || true

echo "QEMU started (PID $(cat "${PID_FILE}"))"

# --- Wait for SSH ---
echo "Waiting for VM to become SSH-reachable (port ${SSH_PORT})..."
wait_for_ssh "after launch"
echo "VM is SSH-reachable."

# --- Wait for cloud-init ---
echo "Waiting for cloud-init to complete..."
ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
    "sudo cloud-init status --wait" 2>/dev/null || true

ensure_iommu_cmdline



echo "VM is ready."
