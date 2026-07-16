#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs inside the QEMU VM (via SSH) to set up SR-IOV VFs, start k3s,
# import the openperouter container image, and deploy openperouter.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SSH_PORT="${QEMU_SSH_PORT:-2222}"
K8S_PORT="${QEMU_K8S_PORT:-6443}"
IMG_TAG="${IMG_TAG:-main-grout}"
IMG_REPO="${IMG_REPO:-quay.io/openperouter}"
IMG_NAME="${IMG_NAME:-router}"
IMG="${IMG_REPO}/${IMG_NAME}:${IMG_TAG}"
OPENPEROUTER_IMAGE_TAR="${OPENPEROUTER_IMAGE_TAR:-}"
KUSTOMIZE_LAYER="${KUSTOMIZE_LAYER:-grout}"
NAMESPACE="${NAMESPACE:-openperouter-system}"

SSH_KEY="${SCRIPT_DIR}/qemu-ssh-key"
SSH_CMD="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ${SSH_KEY} -p ${SSH_PORT} openperouter@localhost"

run_in_vm() {
    ${SSH_CMD} "sudo bash -c '$*'"
}

echo "=== Setting up QEMU VM ==="

# --- Create SR-IOV VFs on the igb PF ---
echo "Creating SR-IOV VFs..."
run_in_vm '
PF=""
for dev in /sys/class/net/*/device/sriov_totalvfs; do
    dir=$(dirname $(dirname "$dev"))
    iface=$(basename "$dir")
    total=$(cat "$dev")
    if [ "$total" -gt 0 ]; then
        PF="$iface"
        break
    fi
done

if [ -z "$PF" ]; then
    echo "ERROR: No SR-IOV capable NIC found" >&2
    exit 1
fi

echo "Found SR-IOV PF: $PF (total VFs: $total)"
echo 2 > /sys/class/net/$PF/device/sriov_numvfs
sleep 2
ls /sys/class/net/$PF/device/virtfn* -d 2>/dev/null || {
    echo "ERROR: VFs not created" >&2
    exit 1
}
echo "VFs created successfully."
'

# --- Bind VF to vfio-pci ---
echo "Binding VF to vfio-pci..."
run_in_vm '
modprobe vfio-pci
echo 1 > /sys/module/vfio/parameters/enable_unsafe_noiommu_mode

PF=""
for dev in /sys/class/net/*/device/sriov_numvfs; do
    numvfs=$(cat "$dev")
    if [ "$numvfs" -gt 0 ]; then
        dir=$(dirname $(dirname "$dev"))
        PF=$(basename "$dir")
        break
    fi
done

VF_PCI=$(basename $(readlink /sys/class/net/$PF/device/virtfn0))
echo "Binding VF $VF_PCI to vfio-pci..."

if [ -e /sys/bus/pci/devices/$VF_PCI/driver ]; then
    echo "$VF_PCI" > /sys/bus/pci/devices/$VF_PCI/driver/unbind
fi
echo "vfio-pci" > /sys/bus/pci/devices/$VF_PCI/driver_override
echo "$VF_PCI" > /sys/bus/pci/drivers/vfio-pci/bind
echo "VF $VF_PCI bound to vfio-pci."
'

# --- Configure the PF underlay IP ---
echo "Configuring PF underlay IP..."
run_in_vm '
PF=""
for dev in /sys/class/net/*/device/sriov_numvfs; do
    numvfs=$(cat "$dev")
    if [ "$numvfs" -gt 0 ]; then
        dir=$(dirname $(dirname "$dev"))
        PF=$(basename "$dir")
        break
    fi
done
ip addr add 192.168.100.10/24 dev $PF 2>/dev/null || true
ip link set $PF up
'

# --- Start k3s ---
echo "Starting k3s..."
run_in_vm 'systemctl start k3s'

echo "Waiting for k3s to be ready..."
RETRIES=60
for i in $(seq 1 $RETRIES); do
    if ${SSH_CMD} "sudo k3s kubectl get nodes" 2>/dev/null | grep -q " Ready"; then
        echo "k3s is ready."
        break
    fi
    if [[ "$i" -eq "$RETRIES" ]]; then
        echo "ERROR: k3s did not become ready within $((RETRIES * 5))s" >&2
        exit 1
    fi
    sleep 5
done

# --- Import the openperouter container image ---
if [[ -n "${OPENPEROUTER_IMAGE_TAR}" && -f "${OPENPEROUTER_IMAGE_TAR}" ]]; then
    echo "Importing openperouter image into k3s..."
    scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "${SSH_KEY}" \
        -P "${SSH_PORT}" "${OPENPEROUTER_IMAGE_TAR}" openperouter@localhost:/tmp/openperouter.tar
    run_in_vm 'k3s ctr images import /tmp/openperouter.tar && rm -f /tmp/openperouter.tar'
    echo "Image imported."
else
    echo "No image tar specified (OPENPEROUTER_IMAGE_TAR), skipping import."
fi

# --- Extract kubeconfig ---
echo "Extracting kubeconfig..."
${SSH_CMD} "sudo cat /etc/rancher/k3s/k3s.yaml" 2>/dev/null \
    | sed "s|https://127.0.0.1:6443|https://127.0.0.1:${K8S_PORT}|g" \
    > "${SCRIPT_DIR}/kubeconfig"
echo "Kubeconfig saved to ${SCRIPT_DIR}/kubeconfig"

# --- Deploy openperouter ---
echo "Deploying openperouter with kustomize layer '${KUSTOMIZE_LAYER}'..."
export KUBECONFIG="${SCRIPT_DIR}/kubeconfig"

KUSTOMIZE="${KUSTOMIZE:-kustomize}"
KUBECTL="${KUBECTL:-kubectl}"

${KUBECTL} create ns "${NAMESPACE}" 2>/dev/null || true
${KUBECTL} label ns "${NAMESPACE}" pod-security.kubernetes.io/enforce=privileged --overwrite

cd "${SCRIPT_DIR}/../../"
cd config/pods && ${KUSTOMIZE} edit set image router="${IMG}"
cd ../../
${KUSTOMIZE} build "config/${KUSTOMIZE_LAYER}" | ${KUBECTL} apply -f -

echo "Waiting for openperouter pods to appear..."
RETRIES=30
for i in $(seq 1 $RETRIES); do
    POD_COUNT=$(${KUBECTL} -n "${NAMESPACE}" get pods --no-headers 2>/dev/null | wc -l)
    if [[ "${POD_COUNT}" -gt 0 ]]; then
        echo "Found ${POD_COUNT} pod(s)."
        break
    fi
    if [[ "$i" -eq "$RETRIES" ]]; then
        echo "WARNING: No pods found after $((RETRIES * 5))s, checking resources..."
        ${KUBECTL} -n "${NAMESPACE}" get all
    fi
    sleep 5
done

echo "Waiting for openperouter pods to be ready..."
${KUBECTL} -n "${NAMESPACE}" wait --for=condition=Ready --all pods --timeout=300s

echo "=== QEMU VM setup complete ==="
