#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Runs inside the QEMU VM (via SSH) to configure underlay networking,
# start k3s, import the openperouter container image, and deploy openperouter.

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

# --- Configure the first igb NIC with the underlay IP ---
echo "Configuring underlay IP on first igb NIC..."
run_in_vm '
UNDERLAY=""
for dev in /sys/class/net/*/device/driver; do
    driver=$(basename $(readlink "$dev"))
    if [ "$driver" = "igb" ]; then
        dir=$(dirname $(dirname "$dev"))
        UNDERLAY=$(basename "$dir")
        break
    fi
done

if [ -z "$UNDERLAY" ]; then
    echo "ERROR: No igb NIC found" >&2
    exit 1
fi

echo "Configuring underlay on $UNDERLAY"
nmcli device set $UNDERLAY managed no 2>/dev/null || true
ip addr add 192.168.100.10/24 dev $UNDERLAY 2>/dev/null || true
ip link set $UNDERLAY up
'

# --- Bind the second igb NIC to vfio-pci (fake VF for DPDK) ---
echo "Binding second igb NIC to vfio-pci..."
run_in_vm '
modprobe vfio-pci
echo 1 > /sys/module/vfio/parameters/enable_unsafe_noiommu_mode

UNDERLAY_PCI=""
VFIO_PCI=""
for pci in /sys/bus/pci/drivers/igb/0000:*; do
    addr=$(basename "$pci")
    if [ -z "$UNDERLAY_PCI" ]; then
        UNDERLAY_PCI="$addr"
        continue
    fi
    VFIO_PCI="$addr"
    break
done

if [ -z "$VFIO_PCI" ]; then
    echo "ERROR: Could not find second igb NIC for vfio-pci binding" >&2
    exit 1
fi

echo "Binding $VFIO_PCI to vfio-pci (underlay PF is $UNDERLAY_PCI)"
echo "$VFIO_PCI" > /sys/bus/pci/drivers/igb/unbind
echo "vfio-pci" > "/sys/bus/pci/devices/$VFIO_PCI/driver_override"
echo "$VFIO_PCI" > /sys/bus/pci/drivers/vfio-pci/bind
echo "Done: $VFIO_PCI is now bound to vfio-pci"
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

cd "${SCRIPT_DIR}/../../../"
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

echo "Waiting for controller and nodemarker pods to be ready..."
${KUBECTL} -n "${NAMESPACE}" wait --for=condition=Ready pods -l app=controller --timeout=300s
${KUBECTL} -n "${NAMESPACE}" wait --for=condition=Ready pods -l app=nodemarker --timeout=300s

if [[ "${KUSTOMIZE_LAYER}" == "grout" ]]; then
    echo "Checking router pod status (grout may not be ready in emulated env)..."
    ${KUBECTL} -n "${NAMESPACE}" get pods -l app=router -o wide || true
else
    echo "Waiting for router pods to be ready..."
    ${KUBECTL} -n "${NAMESPACE}" wait --for=condition=Ready pods -l app=router --timeout=300s
fi

echo "=== QEMU VM setup complete ==="
