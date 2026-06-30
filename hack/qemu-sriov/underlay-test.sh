#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# Creates an Underlay that targets the first SR-IOV VF of the emulated PF and
# checks that the controller binds the VF to a DPDK driver and creates a grout
# port with the VF's PCI BDF address as devargs, configured with the IP that
# was assigned to the VF before creating the Underlay.

NS="${NAMESPACE:-openperouter-system}"
TIMEOUT="${UNDERLAY_TIMEOUT:-120}"
VF_IP="192.168.100.10/24"

# Resolve the first VF's interface name and PCI address on the node.
read -r VF_IFACE VF_PCI < <(ssh_cmd '
  for d in /sys/bus/pci/devices/*/; do
    if [ -f "${d}sriov_totalvfs" ]; then
      vf=$(basename "$(readlink -f "${d}virtfn0")")
      iface=$(basename /sys/bus/pci/devices/"$vf"/net/* 2>/dev/null)
      echo "$iface $vf"
      break
    fi
  done' | tr -d '\r')

if [ -z "$VF_IFACE" ] || [ "$VF_IFACE" = "*" ]; then
  echo "could not determine the first VF interface name on the node" >&2
  exit 1
fi
echo "First VF on the node: $VF_IFACE (PCI: $VF_PCI)"

# Sanity: the VF starts in the host root netns with a kernel driver.
if ! ssh_cmd "ip link show $VF_IFACE >/dev/null 2>&1"; then
  echo "expected $VF_IFACE to start in the host root netns" >&2
  exit 1
fi

# Assign an IP to the VF before creating the Underlay — the controller should
# pick it up and configure the grout port with it.
echo "Assigning $VF_IP to $VF_IFACE..."
ssh_cmd "sudo ip addr add $VF_IP dev $VF_IFACE && sudo ip link set $VF_IFACE up"

# Apply an Underlay targeting the VF.
echo "Creating Underlay targeting $VF_IFACE..."
ssh_cmd "sudo k3s kubectl apply -f -" <<EOF
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay
  namespace: $NS
spec:
  asn: 64514
  tunnelEndpoint:
    cidrs:
      - 100.65.0.0/24
  nics:
    - $VF_IFACE
  neighbors:
    - asn: 64512
      address: 192.168.11.2
EOF

# Wait for the controller to bind the VF to DPDK and create a grout port.
echo "Waiting up to ${TIMEOUT}s for grout port with PCI devargs $VF_PCI..."
deadline=$((SECONDS + TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  # Look for a grout port whose devargs contain the VF's PCI address.
  port_json=$(ssh_cmd "sudo k3s kubectl -n $NS exec deploy/controller -- grcli -s /run/grout.sock interface show 2>/dev/null" || true)
  if echo "$port_json" | grep -q "$VF_PCI"; then
    echo "OK: found grout port with PCI devargs $VF_PCI"

    # The VF kernel netdev should be gone (bound to vfio-pci).
    if ssh_cmd "ip link show $VF_IFACE >/dev/null 2>&1"; then
      echo "unexpected: $VF_IFACE is still present as a kernel netdev" >&2
      exit 1
    fi
    echo "OK: $VF_IFACE is no longer a kernel netdev (bound to DPDK driver)"

    # Check that the grout port has the VF's IP.
    vf_ip_bare="${VF_IP%/*}"
    addr_json=$(ssh_cmd "sudo k3s kubectl -n $NS exec deploy/controller -- grcli -s /run/grout.sock ip address show 2>/dev/null" || true)
    if echo "$addr_json" | grep -q "$vf_ip_bare"; then
      echo "OK: grout port has IP $vf_ip_bare"
    else
      echo "grout port does not have expected IP $vf_ip_bare" >&2
      echo "grout addresses: $addr_json" >&2
      exit 1
    fi

    echo "Underlay VF test passed."
    exit 0
  fi
  sleep 3
done

echo "Timed out waiting for grout port with PCI devargs $VF_PCI" >&2
echo "::group::underlay test diagnostics" >&2
ssh_cmd "sudo k3s kubectl -n $NS get underlay -o yaml" >&2 || true
ssh_cmd "sudo k3s kubectl -n $NS get pods -o wide" >&2 || true
ssh_cmd "sudo k3s kubectl -n $NS logs -l app.kubernetes.io/component=controller --all-containers --prefix --tail=150" >&2 || true
ssh_cmd "sudo k3s kubectl -n $NS exec deploy/controller -- grcli -s /run/grout.sock interface show" >&2 || true
ssh_cmd "echo '--- host links ---'; ip -br link show" >&2 || true
echo "::endgroup::" >&2
exit 1
