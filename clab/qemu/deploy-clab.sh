#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Deploys the containerlab topology for the QEMU E2E environment.
# Only runs the subset of clab/setup.sh steps needed for QEMU:
# bridge creation, leaf config generation, and containerlab deploy.
# Idempotent: safe to re-run — skips deploy if topology is already up.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLAB_DIR="${SCRIPT_DIR}/.."
CLAB_NAME="${CLAB_NAME:-kind}"

# Skip if topology is already deployed and all containers are running
if sudo containerlab inspect --name "${CLAB_NAME}" &>/dev/null 2>&1; then
    echo "Containerlab topology '${CLAB_NAME}' is already deployed, skipping."
    exit 0
fi

# Clean up stale veth pairs from a previous partial deploy
for iface in pf0-up pf1-up; do
    sudo ip link del "${iface}" 2>/dev/null || true
done

# Create all bridges referenced as kind: bridge in the topology
for br in leafkind1-sw leafkind2-sw toleafkind1 toswitch1; do
    if [[ ! -d "/sys/class/net/${br}" ]]; then
        echo "Creating bridge ${br}"
        sudo ip link add name "${br}" type bridge
    fi
    sudo ip link set dev "${br}" up
done

# Generate leaf FRR configs
"${CLAB_DIR}/scripts/02-leaf-configs.sh"

# Deploy containerlab topology
export CLAB_TOPOLOGY="qemu/kind.clab.yml"
"${CLAB_DIR}/scripts/04-containerlab-deploy.sh"
