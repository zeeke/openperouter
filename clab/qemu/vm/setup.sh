#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Bootstraps the k3s cluster inside the QEMU VM: starts k3s, extracts
# kubeconfig, deploys FRR-k8s, Multus, and CNI plugins.
#
# FRR-k8s and Multus must be deployed here (not via clab/setup.sh) because
# clab/setup.sh uses kind-specific mechanisms (kind get nodes, docker cp)
# that don't work with k3s-in-QEMU.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${SCRIPT_DIR}/../../.."

SSH_PORT="${QEMU_SSH_PORT:-2222}"
K8S_PORT="${QEMU_K8S_PORT:-6443}"
MULTUS_VERSION="${MULTUS_VERSION:-v4.2.1}"
CNI_PLUGINS_VERSION=${CNI_PLUGINS_VERSION:-"v1.9.2-0.20260803142000-012159164d7f"}

SSH_KEY="${SCRIPT_DIR}/qemu-vm-key"
SSH_CMD="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i ${SSH_KEY} -p ${SSH_PORT} openperouter@localhost"

run_in_vm() {
    ${SSH_CMD} "sudo bash -c '$*'"
}

echo "=== Bootstrapping QEMU VM cluster ==="

# --- Rename igb NICs to match the bridge they're connected to ---
# Uses udev rules written by cloud-init (70-persistent-net.rules).
# On first boot the NICs are already up with kernel names, so we 
# re-trigger udev, and let the NAME= rules rename them.
echo "Renaming igb NICs..."
run_in_vm '
if ! ip link show toswitch1v0 &>/dev/null; then
  udevadm trigger --action=add --subsystem-match=net
  udevadm settle
else
  echo "  NICs already renamed, skipping."
fi
'

# --- Configure the underlay NIC ---
echo "Configuring underlay IP on igb NIC..."
UNDERLAY="${QEMU_UNDERLAY_NIC:-toswitch1v0}"
run_in_vm "
echo \"Configuring underlay on ${UNDERLAY}\"
nmcli device set ${UNDERLAY} managed no 2>/dev/null || true
ip addr add 192.168.11.3/24 dev ${UNDERLAY} 2>/dev/null || true
ip addr add 2001:db8:11::3/64 dev ${UNDERLAY} 2>/dev/null || true
ip link set ${UNDERLAY} up
"

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

# --- Extract kubeconfig to the standard path ---
KUBECONFIG_PATH="${KUBECONFIG_PATH:-${REPO_ROOT}/bin/kubeconfig}"
echo "Extracting kubeconfig..."
mkdir -p "$(dirname "${KUBECONFIG_PATH}")"
${SSH_CMD} "sudo cat /etc/rancher/k3s/k3s.yaml" 2>/dev/null \
    | sed "s|https://127.0.0.1:6443|https://127.0.0.1:${K8S_PORT}|g" \
    > "${KUBECONFIG_PATH}"
echo "Kubeconfig saved to ${KUBECONFIG_PATH}"

export KUBECONFIG="${KUBECONFIG_PATH}"

KUBECTL="${KUBECTL:-kubectl}"
SCP_CMD="scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i ${SSH_KEY} -P ${SSH_PORT}"
FRR_K8S_DIR="${REPO_ROOT}/clab/kind/frr-k8s"

# --- Deploy FRR-K8s and Multus (same kustomization/manifests as the kind flow) ---
echo "Deploying FRR-K8s..."
${KUBECTL} apply -k "${FRR_K8S_DIR}"

echo "Deploying Multus CNI..."
${KUBECTL} apply -f "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/refs/tags/${MULTUS_VERSION}/deployments/multus-daemonset.yml"

# --- Install CNI plugins (same versions as clab/kind/frr-k8s/setup.sh, via SCP instead of docker cp) ---
echo "Creating CNI symlinks for k3s paths..."
run_in_vm 'mkdir -p /etc/cni /opt/cni'
run_in_vm 'ln -sfn /var/lib/rancher/k3s/agent/etc/cni/net.d /etc/cni/net.d'
run_in_vm 'ln -sfn /var/lib/rancher/k3s/data/cni /opt/cni/bin'

echo "Building CNI plugins from source..."
TEMP_GOBIN=$(mktemp -d)
GOBIN=$TEMP_GOBIN go install github.com/containernetworking/plugins/plugins/main/macvlan@${CNI_PLUGINS_VERSION}
GOBIN=$TEMP_GOBIN go install github.com/containernetworking/plugins/plugins/main/bridge@${CNI_PLUGINS_VERSION}
GOBIN=$TEMP_GOBIN go install github.com/containernetworking/plugins/plugins/ipam/static@${CNI_PLUGINS_VERSION}

echo "Copying CNI plugins to VM..."
for plugin in macvlan bridge static; do
    ${SCP_CMD} "${TEMP_GOBIN}/${plugin}" openperouter@localhost:/tmp/
    run_in_vm "mv /tmp/${plugin} /opt/cni/bin/${plugin} && chmod +x /opt/cni/bin/${plugin}"
done
rm -rf "${TEMP_GOBIN}"

echo "Waiting for FRR-K8s pods to be ready..."
${KUBECTL} -n frr-k8s-system wait --for=condition=Ready --all pods --timeout=300s

echo "Waiting for Multus pods to be ready..."
${KUBECTL} -n kube-system wait --for=condition=Ready pods -l name=multus --timeout=300s

echo "=== QEMU VM cluster bootstrap complete ==="
