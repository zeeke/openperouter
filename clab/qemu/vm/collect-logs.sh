#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Collects diagnostic logs from the QEMU VM and clab FRR containers.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

SSH_PORT="${QEMU_SSH_PORT:-2222}"
CLAB_NAME="${CLAB_NAME:-kind}"
LOG_DIR="${KIND_EXPORT_LOGS:-/tmp/kind_logs}"
KUBECONFIG="${SCRIPT_DIR}/kubeconfig"

SSH_KEY="${SCRIPT_DIR}/qemu-vm-key"
chmod 600 "${SSH_KEY}"
SSH_CMD="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i ${SSH_KEY} -p ${SSH_PORT} openperouter@localhost"

mkdir -p "${LOG_DIR}/qemu-vm"

echo "Collecting QEMU VM logs..."

# VM serial console log
if [[ -f "${SCRIPT_DIR}/serial.log" ]]; then
    sudo cp "${SCRIPT_DIR}/serial.log" "${LOG_DIR}/qemu-vm/serial.log" || true
fi

# In-VM logs (best effort)
if ${SSH_CMD} true 2>/dev/null; then
    echo "  journalctl..."
    ${SSH_CMD} "sudo journalctl --no-pager -l" > "${LOG_DIR}/qemu-vm/journalctl.log" 2>/dev/null || true

    echo "  k3s logs..."
    ${SSH_CMD} "sudo journalctl -u k3s --no-pager -l" > "${LOG_DIR}/qemu-vm/k3s.log" 2>/dev/null || true

    echo "  dmesg..."
    ${SSH_CMD} "sudo dmesg" > "${LOG_DIR}/qemu-vm/dmesg.log" 2>/dev/null || true

    echo "  PCI devices..."
    ${SSH_CMD} "sudo lspci -vvv" > "${LOG_DIR}/qemu-vm/lspci.log" 2>/dev/null || true
    ${SSH_CMD} "sudo lspci -k" > "${LOG_DIR}/qemu-vm/lspci-k.log" 2>/dev/null || true

    echo "  SR-IOV info..."
    ${SSH_CMD} 'for d in /sys/class/net/*/device/sriov_numvfs; do
        iface=$(basename $(dirname $(dirname "$d")))
        echo "=== $iface ==="
        echo "sriov_numvfs: $(cat $d)"
        cat $(dirname $d)/sriov_totalvfs 2>/dev/null && echo "sriov_totalvfs: $(cat $(dirname $d)/sriov_totalvfs)" || true
    done' > "${LOG_DIR}/qemu-vm/sriov-info.log" 2>/dev/null || true

    echo "  ip addr/route..."
    ${SSH_CMD} "ip addr show" > "${LOG_DIR}/qemu-vm/ip-addr.log" 2>/dev/null || true
    ${SSH_CMD} "ip route show" > "${LOG_DIR}/qemu-vm/ip-route.log" 2>/dev/null || true

    # kubectl logs for openperouter pods
    if [[ -f "${KUBECONFIG}" ]]; then
        echo "  openperouter pod logs..."
        export KUBECONFIG
        KUBECTL="${KUBECTL:-kubectl}"
        for pod in $(${KUBECTL} -n openperouter-system get pods -o name 2>/dev/null || true); do
            pod_name=$(basename "${pod}")
            ${KUBECTL} -n openperouter-system logs "${pod}" --all-containers --ignore-errors \
                > "${LOG_DIR}/qemu-vm/pod-${pod_name}.log" 2>/dev/null || true
        done

        echo "  kubectl describe pods..."
        ${KUBECTL} -n openperouter-system describe pods \
            > "${LOG_DIR}/qemu-vm/describe-pods.log" 2>/dev/null || true

        echo "  cluster-info dump..."
        ${KUBECTL} cluster-info dump --output-directory="${LOG_DIR}/qemu-vm/cluster-state" \
            --all-namespaces 2>/dev/null || true
    fi
else
    echo "  WARNING: VM not reachable via SSH, skipping in-VM log collection."
fi

# Clab FRR container logs
echo "Collecting clab FRR logs..."
for node in leafkind1 leafkind2 spine leafA leafB leafSRV6; do
    container="clab-${CLAB_NAME}-${node}"
    if docker inspect "${container}" &>/dev/null; then
        docker logs "${container}" > "${LOG_DIR}/qemu-vm/${node}.log" 2>&1 || true
        docker exec "${container}" vtysh -c "show bgp summary" \
            > "${LOG_DIR}/qemu-vm/${node}-bgp-summary.log" 2>/dev/null || true
        docker exec "${container}" vtysh -c "show running-config" \
            > "${LOG_DIR}/qemu-vm/${node}-running-config.log" 2>/dev/null || true
    fi
done

echo "Logs collected in ${LOG_DIR}/qemu-vm/"
