#!/bin/bash
# Execute setup scripts in containers
set -euo pipefail

source "$(dirname $(readlink -f $0))/../common.sh"

# Get cluster names from arguments
CLUSTER_NAMES=("$@")

if [[ ${#CLUSTER_NAMES[@]} -eq 0 ]]; then
    echo "Usage: $0 <cluster_name> [cluster_name2] ..."
    echo "Example: $0 pe-kind"
    echo "Example: $0 pe-kind-a pe-kind-b"
    exit 1
fi

set_rp_filter() {
    # Some k8s vendors set net.ipv4.conf.default.rp_filter=1 (strict mode) on all
    # nodes. New network namespaces (including "perouter") inherit their sysctl
    # defaults from the host's init_net, so we must set it here.
    #
    # The kernel computes the effective rp_filter as
    #   max(conf/all/rp_filter, conf/<iface>/rp_filter)
    # so conf/all must be 0; otherwise the per-interface DisableRPFilter(0)
    # that the controller applies to grout ports would have no effect.
    echo "Setting rp_filter to strict mode"
    sudo sysctl -w net.ipv4.conf.default.rp_filter=1
    sudo sysctl -w net.ipv4.conf.all.rp_filter=0

    sudo sysctl -w net.ipv4.conf.default.log_martians=1
    sudo sysctl -w net.ipv4.conf.all.log_martians=1

    for cluster_name in "${CLUSTER_NAMES[@]}"; do
        local nodes
        nodes=$(${KIND_COMMAND} get nodes --name "${cluster_name}" 2>/dev/null) || continue
        for node in $nodes; do
            ${CONTAINER_ENGINE_CLI} exec "${node}" sysctl -w \
                net.ipv4.conf.default.rp_filter=1 \
                net.ipv4.conf.all.rp_filter=0
        done
    done
}

# Set rp_filter before container setup so any namespace created later
# inherits strict-mode defaults.
set_rp_filter

setup_containers() {
    echo "Executing setup scripts in containers for clusters: ${CLUSTER_NAMES[*]}"

    # Setup common leaf containers (always present)
    ${CONTAINER_ENGINE_CLI} exec clab-kind-leafA /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-leafB /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-leafSRV6 /setup.sh

    # Setup host containers (always present)
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostA_red /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostA_blue /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostA_default /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostB_red /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostB_blue /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostSRV6_red /setup.sh
    ${CONTAINER_ENGINE_CLI} exec clab-kind-hostSRV6_blue /setup.sh

    # Setup cluster-specific leaf containers
    for cluster_name in "${CLUSTER_NAMES[@]}"; do
        if [[ ${#CLUSTER_NAMES[@]} -eq 1 ]]; then
            # Single cluster mode - try leafkind containers
            for leaf in leafkind1 leafkind2; do
                if ${CONTAINER_ENGINE_CLI} exec clab-kind-${leaf} test -f /setup.sh 2>/dev/null; then
                    echo "Setting up ${leaf} container"
                    ${CONTAINER_ENGINE_CLI} exec clab-kind-${leaf} /setup.sh
                fi
            done
        else
            # Multi-cluster mode - try cluster-specific leafkind containers
            cluster_suffix="${cluster_name##*-}"
            container_name="clab-kind-leafkind-${cluster_suffix}"
            if ${CONTAINER_ENGINE_CLI} exec ${container_name} test -f /setup.sh 2>/dev/null; then
                echo "Setting up ${container_name} container"
                ${CONTAINER_ENGINE_CLI} exec ${container_name} /setup.sh
            fi
        fi
    done
}

setup_containers
