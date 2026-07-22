#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Starts an FRR container acting as a TOR switch mock, connected to the
# br-underlay bridge so it can peer with the QEMU VM via BGP.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BRIDGE_NAME="${QEMU_BRIDGE:-br-underlay}"
FRR_CONTAINER_NAME="${QEMU_FRR_CONTAINER:-qemu-tor}"
FRR_IMAGE="${QEMU_FRR_IMAGE:-quay.io/frrouting/frr:10.6.0}"
TOR_IP="${QEMU_TOR_IP:-192.168.100.1/24}"

# Remove stale container if present.
if docker inspect "${FRR_CONTAINER_NAME}" &>/dev/null; then
    docker rm -f "${FRR_CONTAINER_NAME}"
fi

echo "Starting FRR TOR container (${FRR_CONTAINER_NAME})..."

docker run -d \
    --name "${FRR_CONTAINER_NAME}" \
    --privileged \
    --network none \
    -v "${SCRIPT_DIR}/frr/frr.conf:/etc/frr/frr.conf:ro" \
    -v "${SCRIPT_DIR}/frr/daemons:/etc/frr/daemons:ro" \
    "${FRR_IMAGE}"

# Connect the container to br-underlay.
# We create a veth pair: one end goes into the container, the other joins the bridge.
VETH_HOST="veth-tor"
VETH_PEER="veth-tor-c"
VETH_CONTAINER="eth0"

# Get the container's network namespace PID.
TOR_PID=$(docker inspect -f '{{.State.Pid}}' "${FRR_CONTAINER_NAME}")

# Create veth pair (use a temp name for the peer to avoid clashing with host eth0).
if ip link show "${VETH_HOST}" &>/dev/null; then
    sudo ip link del "${VETH_HOST}"
fi
sudo ip link add "${VETH_HOST}" type veth peer name "${VETH_PEER}"

# Move the peer into the container's netns and rename it to eth0.
sudo ip link set "${VETH_PEER}" netns "${TOR_PID}"
sudo nsenter -t "${TOR_PID}" -n ip link set "${VETH_PEER}" name "${VETH_CONTAINER}"

# Attach the host end to the bridge.
sudo ip link set "${VETH_HOST}" master "${BRIDGE_NAME}"
sudo ip link set "${VETH_HOST}" up

# Configure the container end.
sudo nsenter -t "${TOR_PID}" -n ip addr add "${TOR_IP}" dev "${VETH_CONTAINER}"
sudo nsenter -t "${TOR_PID}" -n ip link set "${VETH_CONTAINER}" up
sudo nsenter -t "${TOR_PID}" -n ip link set lo up

echo "FRR TOR container is running with IP ${TOR_IP} on ${BRIDGE_NAME}."
