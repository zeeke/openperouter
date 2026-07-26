#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Starts an FRR container acting as a TOR switch mock, connected to the
# br-underlay bridge so it can peer with the QEMU VM via BGP.
#
# The TOR is configured with:
#   - BGP (all address families: ipv4/ipv6 unicast, l2vpn evpn, ipv4/ipv6 vpn)
#   - ISIS level-1
#   - SRv6 with a uSID locator
#   - VRFs red (VNI 100) and blue (VNI 200) with VXLAN/bridges
#   - Static routes in each VRF to simulate external hosts

set -euo pipefail

set -x

sudo lsmod 
sudo modprobe vrf

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BRIDGE_NAME="${QEMU_BRIDGE:-br-underlay}"
FRR_CONTAINER_NAME="${QEMU_FRR_CONTAINER:-qemu-tor}"
FRR_IMAGE="${QEMU_FRR_IMAGE:-quay.io/frrouting/frr:10.6.0}"
TOR_IP="${QEMU_TOR_IP:-192.168.100.1/24}"
TOR_IPV6="${QEMU_TOR_IPV6:-2001:db8:100::1/64}"

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

in_tor_ns() {
    sudo nsenter -t "${TOR_PID}" -n "$@"
}

# Create veth pair (use a temp name for the peer to avoid clashing with host eth0).
if ip link show "${VETH_HOST}" &>/dev/null; then
    sudo ip link del "${VETH_HOST}"
fi
sudo ip link add "${VETH_HOST}" type veth peer name "${VETH_PEER}"

# Move the peer into the container's netns and rename it to eth0.
sudo ip link set "${VETH_PEER}" netns "${TOR_PID}"
in_tor_ns ip link set "${VETH_PEER}" name "${VETH_CONTAINER}"

# Attach the host end to the bridge.
sudo ip link set "${VETH_HOST}" master "${BRIDGE_NAME}"
sudo ip link set "${VETH_HOST}" up

# Configure the container end with IPv4 and IPv6.
in_tor_ns ip addr add "${TOR_IP}" dev "${VETH_CONTAINER}"
in_tor_ns ip addr add "${TOR_IPV6}" dev "${VETH_CONTAINER}"
in_tor_ns ip link set "${VETH_CONTAINER}" up
in_tor_ns ip link set lo up

# --- Loopback addresses (VTEP + SRv6 source) ---
in_tor_ns ip addr add 100.64.0.1/32 dev lo
in_tor_ns ip addr add 2001:db8:1234::1/128 dev lo

# --- SRv6 dummy interface (required by ISIS SRv6 SID installation) ---
in_tor_ns ip link add sr0 type dummy
in_tor_ns ip link set sr0 up

# --- IPv6 forwarding and SRv6 ---
in_tor_ns sysctl -qw net.ipv6.conf.all.forwarding=1
in_tor_ns sysctl -qw net.ipv6.conf.all.seg6_enabled=1
in_tor_ns sysctl -qw net.ipv6.conf.eth0.seg6_enabled=1
in_tor_ns sysctl -qw net.ipv6.conf.lo.seg6_enabled=1
in_tor_ns sysctl -qw net.ipv6.conf.sr0.seg6_enabled=1

# --- VRFs ---
#in_tor_ns sysctl -qw net.vrf.strict_mode=1 2>/dev/null || true
in_tor_ns ip link add red type vrf table 1100
in_tor_ns ip link set red up
in_tor_ns ip link add blue type vrf table 1200
in_tor_ns ip link set blue up

# --- VXLAN interfaces ---
in_tor_ns ip link add vxlan100 type vxlan id 100 local 100.64.0.1 dstport 4789 nolearning
in_tor_ns ip link add vxlan200 type vxlan id 200 local 100.64.0.1 dstport 4789 nolearning

# --- Bridges for VXLAN ---
in_tor_ns ip link add br100 type bridge
in_tor_ns ip link set br100 master red
in_tor_ns ip link set br100 addrgenmode none
in_tor_ns ip link set vxlan100 master br100
in_tor_ns ip link set br100 up
in_tor_ns ip link set vxlan100 up

in_tor_ns ip link add br200 type bridge
in_tor_ns ip link set br200 master blue
in_tor_ns ip link set br200 addrgenmode none
in_tor_ns ip link set vxlan200 master br200
in_tor_ns ip link set br200 up
in_tor_ns ip link set vxlan200 up

# --- Reload FRR config ---
# FRR reads its config at container startup before networking is set up.
# Re-apply the config now that VRFs, VXLAN, bridges, and sr0 are ready.
echo "Reloading FRR config..."
sleep 2
docker exec "${FRR_CONTAINER_NAME}" vtysh -f /etc/frr/frr.conf

echo "FRR TOR container is running with IP ${TOR_IP} and ${TOR_IPV6} on ${BRIDGE_NAME}."
echo "  VTEP: 100.64.0.1  SRv6 source: 2001:db8:1234::1"
echo "  VRFs: red (VNI 100), blue (VNI 200)"
