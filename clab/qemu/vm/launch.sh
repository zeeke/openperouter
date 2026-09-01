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

# Check if containerlab topology is deployed
if ! sudo containerlab inspect --name "${CLAB_NAME}" &>/dev/null 2>&1; then
    echo "ERROR: Containerlab topology '${CLAB_NAME}' is not deployed." >&2
    echo "  Please run: make qemu-clab" >&2
    exit 1
fi

# --- Networking setup ---
echo "Setting up TAP devices and connecting to containerlab bridges..."


vf_incremental=1

setup_fake_pf() {
    local pfName=$1
    local numVfs=$2

    for br in leafkind1-sw leafkind2-sw toleafkind1 toswitch1; do
        if [[ ! -d "/sys/class/net/${br}" ]]; then
            echo "Creating bridge ${br}"
        fi
        sudo ip link set dev "${br}" up
    done

    
    sudo ip link add name "${pfName}" type bridge
    
    for i in $(seq 0 $((numVfs - 1))); do
        local vfRepresentor="${pfName}v${i}_rep"

        sudo ip tuntap add dev "${vfRepresentor}" mode tap
        sudo ip link set "${vfRepresentor}" master "${pfName}"
        sudo ip link set "${vfRepresentor}" up

        vf_mac=$(printf "52:54:00:ab:cd:%02x" $((vf_incremental + 1)))
        NIC_ARGS="${NIC_ARGS} -device pcie-root-port,id=rp${vf_incremental},slot=${vf_incremental}"
        NIC_ARGS="${NIC_ARGS} -netdev tap,id=nic${vf_incremental},ifname=vfRepresentor,script=no,downscript=no"
        NIC_ARGS="${NIC_ARGS} -device igb,bus=rp${vf_incremental},netdev=nic${vf_incremental},mac=${vf_mac}"

        vf_incremental=$((vf_incremental + 1))
    done

    sudo ip link set dev "${pfName}" up
}


setup_fake_pf "toswitch1" 4
setup_fake_pf "toswitch2" 4
setup_fake_pf "toleafkind1" 4
setup_fake_pf "toleafkind2" 4

# --- Launch QEMU ---
echo "Launching QEMU VM with 8 igb NICs (2 fake PFs)..."

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
