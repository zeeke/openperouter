#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Launches QEMU VM with 8 igb NICs representing 2 fake SR-IOV PFs.
# Each PF has 4 NICs: 2 trunk ports + 2 VLAN access ports (33, 44).
# TAP devices are attached to containerlab-managed bridges.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

VM_IMAGE="${SCRIPT_DIR}/fedora-cloud.qcow2"
CLOUD_INIT_ISO="${SCRIPT_DIR}/cloud-init.iso"
CLAB_NAME="${CLAB_NAME:-kind}"
NUM_NICS=8  # 2 fake PFs × 4 NICs each
SSH_PORT="${QEMU_SSH_PORT:-2222}"
K8S_PORT="${QEMU_K8S_PORT:-6443}"
VM_CPUS="${QEMU_VM_CPUS:-4}"
VM_MEM="${QEMU_VM_MEM:-6144}"
PID_FILE="${SCRIPT_DIR}/qemu.pid"
SERIAL_LOG="${SCRIPT_DIR}/serial.log"
SSH_KEY="${SCRIPT_DIR}/qemu-vm-key"
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

# Check if containerlab topology is deployed
if ! sudo containerlab inspect --name "${CLAB_NAME}" &>/dev/null 2>&1; then
    echo "ERROR: Containerlab topology '${CLAB_NAME}' is not deployed." >&2
    echo "  Please run: make qemu-clab" >&2
    exit 1
fi

# --- Networking setup ---
echo "Setting up TAP devices and connecting to containerlab bridges..."

setup_tap_for_clab() {
    local tap=$1
    local bridge=$2

    if ! ip link show "${tap}" &>/dev/null 2>&1; then
        echo "Creating TAP device ${tap}..."
        sudo ip tuntap add dev "${tap}" mode tap
    fi

    if ip link show "${bridge}" &>/dev/null 2>&1; then
        echo "Attaching ${tap} to ${bridge}..."
        sudo ip link set "${tap}" master "${bridge}"
        sudo ip link set "${tap}" up
    else
        echo "ERROR: Bridge ${bridge} not found!" >&2
        echo "  Make sure containerlab topology is deployed." >&2
        exit 1
    fi
}

# toleafkind1 PF (TAPs 0-3)
echo "Setting up toleafkind1 PF (4 NICs)..."
for i in 0 1 2 3; do
    setup_tap_for_clab "qemu-tap${i}" "toleafkind1"
done

# toswitch1 PF (TAPs 4-7)
echo "Setting up toswitch1 PF (4 NICs)..."
for i in 4 5 6 7; do
    setup_tap_for_clab "qemu-tap${i}" "toswitch1"
done

# --- Configure VLAN filtering on bridges ---
echo "Configuring VLAN filtering on fake PF bridges..."

configure_bridge_vlans() {
    local bridge=$1
    local tap_prefix=$2  # e.g., "0" for toleafkind1 (TAPs 0-3)

    echo "  Configuring ${bridge}..."

    sudo ip link set "${bridge}" type bridge vlan_filtering 1

    # TAPs 0, 1 (or 4, 5): trunk ports
    for i in 0 1; do
        local tap_idx=$((tap_prefix + i))
        sudo bridge vlan add vid 33 dev "qemu-tap${tap_idx}"
        sudo bridge vlan add vid 44 dev "qemu-tap${tap_idx}"
    done

    # TAP 2 (or 6): VLAN 33 access port
    local tap2=$((tap_prefix + 2))
    sudo bridge vlan del vid 1 dev "qemu-tap${tap2}"
    sudo bridge vlan add vid 33 dev "qemu-tap${tap2}" pvid untagged

    # TAP 3 (or 7): VLAN 44 access port
    local tap3=$((tap_prefix + 3))
    sudo bridge vlan del vid 1 dev "qemu-tap${tap3}"
    sudo bridge vlan add vid 44 dev "qemu-tap${tap3}" pvid untagged

    sudo bridge vlan add vid 33 dev "${bridge}" self
    sudo bridge vlan add vid 44 dev "${bridge}" self
}

configure_bridge_vlans "toleafkind1" 0
configure_bridge_vlans "toswitch1" 4

echo "VLAN filtering configured."

# --- Launch QEMU ---
echo "Launching QEMU VM with 8 igb NICs (2 fake PFs)..."

touch "${SERIAL_LOG}" "${PID_FILE}"

NIC_ARGS=""
for i in $(seq 0 $((NUM_NICS - 1))); do
    MAC=$(printf "52:54:00:ab:cd:%02x" $((i + 1)))
    NIC_ARGS="${NIC_ARGS} -device pcie-root-port,id=rp${i},slot=${i}"
    NIC_ARGS="${NIC_ARGS} -netdev tap,id=nic${i},ifname=qemu-tap${i},script=no,downscript=no"
    NIC_ARGS="${NIC_ARGS} -device igb,bus=rp${i},netdev=nic${i},mac=${MAC}"
done

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
