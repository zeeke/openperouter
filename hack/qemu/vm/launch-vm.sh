#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Creates a Linux bridge + TAP devices, launches a QEMU/KVM VM with 4 igb
# NICs on the underlay, and waits for SSH to become reachable.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

VM_IMAGE="${SCRIPT_DIR}/fedora-cloud.qcow2"
CLOUD_INIT_ISO="${SCRIPT_DIR}/cloud-init.iso"
BRIDGE_NAME="${QEMU_BRIDGE:-br-underlay}"
TAP_PREFIX="${QEMU_TAP_PREFIX:-vm-underlay}"
NUM_NICS=4
SSH_PORT="${QEMU_SSH_PORT:-2222}"
K8S_PORT="${QEMU_K8S_PORT:-6443}"
VM_CPUS="${QEMU_VM_CPUS:-4}"
VM_MEM="${QEMU_VM_MEM:-6144}"
PID_FILE="${SCRIPT_DIR}/qemu.pid"
SERIAL_LOG="${SCRIPT_DIR}/serial.log"
SSH_KEY="${SCRIPT_DIR}/qemu-ssh-key"

if [[ ! -f "${VM_IMAGE}" ]]; then
    echo "ERROR: VM image not found at ${VM_IMAGE}. Run 'make qemu-image' first." >&2
    exit 1
fi

if [[ ! -f "${CLOUD_INIT_ISO}" ]]; then
    echo "ERROR: cloud-init ISO not found at ${CLOUD_INIT_ISO}. Run 'make qemu-image' first." >&2
    exit 1
fi

# --- Generate SSH key if needed ---
if [[ ! -f "${SSH_KEY}" ]]; then
    echo "Generating SSH key pair..."
    ssh-keygen -t ed25519 -f "${SSH_KEY}" -N "" -q
fi
SSH_PUBKEY=$(cat "${SSH_KEY}.pub")

# --- Regenerate cloud-init ISO with the SSH key ---
CLOUD_INIT_SRC="${SCRIPT_DIR}/../image/cloud-init"
CLOUD_INIT_TMPDIR=$(mktemp -d)
cp "${CLOUD_INIT_SRC}/meta-data" "${CLOUD_INIT_TMPDIR}/"
sed "s|ssh_authorized_keys: \[\]|ssh_authorized_keys:\n      - ${SSH_PUBKEY}|" \
    "${CLOUD_INIT_SRC}/user-data" > "${CLOUD_INIT_TMPDIR}/user-data"

if command -v genisoimage &>/dev/null; then
    genisoimage -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_TMPDIR}/user-data" "${CLOUD_INIT_TMPDIR}/meta-data" 2>/dev/null
elif command -v mkisofs &>/dev/null; then
    mkisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_TMPDIR}/user-data" "${CLOUD_INIT_TMPDIR}/meta-data" 2>/dev/null
elif command -v xorrisofs &>/dev/null; then
    xorrisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_TMPDIR}/user-data" "${CLOUD_INIT_TMPDIR}/meta-data" 2>/dev/null
fi
rm -rf "${CLOUD_INIT_TMPDIR}"
echo "cloud-init ISO regenerated with SSH key."

# --- Networking setup ---
echo "Setting up bridge ${BRIDGE_NAME} and ${NUM_NICS} TAP devices..."

# Create the bridge if it doesn't exist.
if ! ip link show "${BRIDGE_NAME}" &>/dev/null; then
    sudo ip link add name "${BRIDGE_NAME}" type bridge
    sudo ip link set "${BRIDGE_NAME}" up
    sudo ip addr add 192.168.100.254/24 dev "${BRIDGE_NAME}" 2>/dev/null || true
fi

# Create TAP devices for the VM's underlay NICs (one per igb NIC).
for i in $(seq 0 $((NUM_NICS - 1))); do
    TAP="${TAP_PREFIX}-${i}"
    if ! ip link show "${TAP}" &>/dev/null; then
        sudo ip tuntap add dev "${TAP}" mode tap
        sudo ip link set "${TAP}" master "${BRIDGE_NAME}"
        sudo ip link set "${TAP}" up
    fi
done

# Enable VLAN filtering on the bridge so TAPs 2 and 3 can act as
# access ports for the VF-to-VF fake-VF simulation.
sudo ip link set "${BRIDGE_NAME}" type bridge vlan_filtering 1

# TAPs 0 and 1 (underlay + trunk VF): keep default PVID 1 for untagged
# underlay traffic, add VLANs 33 and 44 as tagged so VLAN-tagged frames
# from the access ports can reach the trunk VF.
for i in 0 1; do
    sudo bridge vlan add vid 33 dev "${TAP_PREFIX}-${i}"
    sudo bridge vlan add vid 44 dev "${TAP_PREFIX}-${i}"
done

# TAP 2 (fake VF for VLAN 33): access port, PVID 33, untagged egress.
sudo bridge vlan del vid 1 dev "${TAP_PREFIX}-2"
sudo bridge vlan add vid 33 dev "${TAP_PREFIX}-2" pvid untagged

# TAP 3 (fake VF for VLAN 44): access port, PVID 44, untagged egress.
sudo bridge vlan del vid 1 dev "${TAP_PREFIX}-3"
sudo bridge vlan add vid 44 dev "${TAP_PREFIX}-3" pvid untagged

# Allow VLANs 33 and 44 on the bridge itself.
sudo bridge vlan add vid 33 dev "${BRIDGE_NAME}" self
sudo bridge vlan add vid 44 dev "${BRIDGE_NAME}" self

# --- Launch QEMU ---
echo "Launching QEMU VM..."

# Pre-create files so they're owned by current user, not root.
touch "${SERIAL_LOG}" "${PID_FILE}"

# Build NIC arguments: 4 igb NICs, each on its own PCIe root port and TAP.
NIC_ARGS=""
for i in $(seq 0 $((NUM_NICS - 1))); do
    MAC=$(printf "52:54:00:ab:cd:%02x" $((i + 1)))
    NIC_ARGS="${NIC_ARGS} -device pcie-root-port,id=rp${i},slot=${i}"
    NIC_ARGS="${NIC_ARGS} -netdev tap,id=underlay${i},ifname=${TAP_PREFIX}-${i},script=no,downscript=no"
    NIC_ARGS="${NIC_ARGS} -device igb,bus=rp${i},netdev=underlay${i},mac=${MAC}"
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

# Fix ownership of files created/overwritten by QEMU (running as root).
sudo chown "$(id -u):$(id -g)" "${SERIAL_LOG}" "${PID_FILE}" 2>/dev/null || true

echo "QEMU started (PID $(cat "${PID_FILE}"))"

# --- Wait for SSH ---
echo "Waiting for VM to become SSH-reachable (port ${SSH_PORT})..."
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i ${SSH_KEY}"
MAX_WAIT=300
ELAPSED=0
while ! ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost true 2>/dev/null; do
    sleep 5
    ELAPSED=$((ELAPSED + 5))
    if [[ "${ELAPSED}" -ge "${MAX_WAIT}" ]]; then
        echo "ERROR: VM did not become SSH-reachable within ${MAX_WAIT}s." >&2
        echo "--- Last 50 lines of serial log ---" >&2
        tail -50 "${SERIAL_LOG}" >&2 || true
        exit 1
    fi
    echo "  still waiting... (${ELAPSED}s / ${MAX_WAIT}s)"
done

echo "VM is SSH-reachable."

# --- Wait for cloud-init to finish ---
echo "Waiting for cloud-init to complete..."
ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
    "sudo cloud-init status --wait" 2>/dev/null || true

# --- Reboot if kernel cmdline changes need to take effect ---
if ! ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
    "grep -q intel_iommu=on /proc/cmdline" 2>/dev/null; then
    echo "Rebooting VM for kernel cmdline changes (IOMMU, hugepages)..."
    ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost \
        "sudo reboot" 2>/dev/null || true
    sleep 10
    ELAPSED=0
    while ! ssh ${SSH_OPTS} -p "${SSH_PORT}" openperouter@localhost true 2>/dev/null; do
        sleep 5
        ELAPSED=$((ELAPSED + 5))
        if [[ "${ELAPSED}" -ge "${MAX_WAIT}" ]]; then
            echo "ERROR: VM did not become SSH-reachable after reboot within ${MAX_WAIT}s." >&2
            exit 1
        fi
        echo "  waiting for reboot... (${ELAPSED}s / ${MAX_WAIT}s)"
    done
    echo "VM is back after reboot."
fi

echo "VM is ready."
