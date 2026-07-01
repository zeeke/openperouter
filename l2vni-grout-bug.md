# Bug: EVPN Type-5 nexthops resolve to the wrong VXLAN interface (L2 VNI instead of L3 VNI)

## Problem

When zebra installs an EVPN Type-5 (IP prefix) route into grout via
`grout_add_nexthop`, the nexthop is associated with the wrong VXLAN interface.
It picks the L2 VNI VXLAN interface (`vni110`) instead of the L3 VNI VXLAN
interface (`vni100`).

This causes inter-subnet routed traffic to be VXLAN-encapsulated with VNI 110
(L2) instead of VNI 100 (L3). The remote PE receives the packet on its L2
bridge domain and cannot L3-route it to the destination — so traffic is
black-holed.

## Setup

Two VNIs in VRF "red":
- **L3 VNI**: VNI 100, grout interface `vni100` (ifindex 33), used for
  inter-subnet routing
- **L2 VNI**: VNI 110, grout interface `vni110` (ifindex 34), used for L2
  bridging within subnet 192.171.24.0/24

The VRF "red" interface itself has ifindex 32.

Remote PE advertises 192.168.20.0/24 as an EVPN Type-5 route with L3 VNI
label 100, nexthop 100.64.0.1, RMAC aa:bb:cc:00:00:65.

## What BGP sends to zebra

```
BGP announcing route 192.168.20.0/24(VRF red) to zebra
bgp_zebra_announce_parse_nexthop: p=192.168.20.0/24, bgp_is_valid_label: 0, num_labels: 1
Tx route add VRF red (table id 0) 192.168.20.0/24 metric 0 tag 0 count 1 nhg 0
  nhop [1]: 100.64.0.1 if 32 VRF 32 wt 0    RMAC aa:bb:cc:00:00:65
```

Note: `if 32` is the VRF interface "red", not a specific VXLAN interface. BGP
expects zebra to resolve which VXLAN carries the L3 VNI for this VRF.

## What zebra's grout plugin does

```
grout_add_del_nexthop: add nh_id 33 origin zebra
grout_add_nexthop: nh_id 33 type L3
grout_client_send_recv: GR_NH_ADD: success
grout_add_del_route: add 192.168.20.0/24 origin bgp nh_id 33 vrf 4
grout_client_send_recv: GR_IP4_ROUTE_ADD: success
```

## What grout's routing table shows (wrong)

```
red   ipv4    192.168.20.0/24       bgp     type=L3 id=33 iface=vni110 vrf= origin=zebra family=ipv4 addr=100.64.0.1 mac=aa:bb:cc:00:00:65 flags=static remote
```

The route points at **`iface=vni110`** (VNI 110, the L2 VNI). It should point
at **`iface=vni100`** (VNI 100, the L3 VNI).

## What happens in the datapath (packet trace)

```
iface_input: iface=br-pe-110 mode=VRF
ip_input: 192.171.24.2 > 192.168.20.2 ttl=64 proto=TCP(6)
ip_forward:
ip_output: 192.171.24.2 > 192.168.20.2 ttl=63 proto=TCP(6)
eth_output: 12:30:9f:af:69:80 > aa:bb:cc:00:00:65 type=IP(0x0800)
iface_output: iface=vni110          <-- WRONG: should be vni100
vxlan_output: vni=110 vtep=100.64.0.1  <-- encapsulated with L2 VNI 110 instead of L3 VNI 100
```

The remote PE at 100.64.0.1 receives this on VNI 110 (its L2 bridge), not
VNI 100 (its L3 VRF). The packet is L2-forwarded instead of L3-routed.
Since 192.168.20.2 is not on the VNI 110 L2 segment, the packet is dropped.
No SYN-ACK ever comes back.

## Expected behavior

When `grout_add_nexthop` processes a Type-5 route with `if 32` (the VRF
interface), it should resolve the output interface to the VXLAN interface
that carries the **L3 VNI** for that VRF — `vni100` (ifindex 33), not
`vni110` (ifindex 34).

The correct routing table entry should be:

```
red   ipv4    192.168.20.0/24       bgp     type=L3 id=33 iface=vni100 vrf=red origin=zebra family=ipv4 addr=100.64.0.1 mac=aa:bb:cc:00:00:65 flags=static remote
```

## Additional errors in the same session

There are also related `cp_set_vrf_master` errors that may share the same
root cause (confusion about which interface is L2 vs L3):

```
ERR: CTLPLANE: cp_set_vrf_master: no VRF iface for id 0 on vni110
ERR: CTLPLANE: cp_set_vrf_master: no VRF iface for id 0 on pe-110
```

And FDB install failures when trying to add remote RMAC entries to the L3
VNI bridge:

```
grout_macfdb_update_ctx: add bridge=5 iface=5 mac=aa:bb:cc:00:00:65 vlan=0 vtep=100.64.0.1
grout_client_send_recv: ERROR: GR_FDB_ADD: Wrong medium type
```

## Where to look

The bug is in the nexthop resolution logic — likely in or around
`grout_add_nexthop` in the zebra grout plugin. When the nexthop interface is
a VRF interface (ifindex 32), the code needs to find the L3 VNI VXLAN
interface for that VRF. It currently picks the wrong one (possibly the last
VXLAN interface created in the VRF, or iterates and doesn't filter by L3 vs
L2 VNI role).
