# IPv6 VTEP Test Failure Analysis — Grout Datapath

## Failed Test

```
[FAILED] IPv6 VTEP should establish pod-to-pod connectivity over L2VNI [It] with IPv6 VTEP [grout-support]
```

Pod1 (192.173.24.2) cannot reach pod2 (192.173.24.3) — curl times out with "Host is unreachable" after 41.6s.

## Root Cause

**FRR version mismatch between PE routers and transit infrastructure.**

The grout datapath builds its own FRR 10.7.0 from source (`Dockerfile.grout`, line 94–99: `make frr-rpm FRR=10.7` downloads `frr-10.7.0.tar.gz`), while the leafkind transit switches and spine still run FRR 10.6.0 (`clab/singlecluster/kind.clab.yml`, line 6: `quay.io/frrouting/frr:10.6.0`).

FRR 10.7.0 includes PR [#21507](https://github.com/FRRouting/frr/pull/21507) (merged 2026-04-14) which fixes PMSI Tunnel Attribute encoding for IPv6 VTEPs in EVPN Type-3 IMET routes. This fix changes the wire format in a way that is incompatible with FRR 10.6.0 receivers.

### FRR version matrix in each test configuration

| Component                    | Kernel datapath           | Grout datapath                          |
|------------------------------|---------------------------|-----------------------------------------|
| PE routers (openperouter)    | FRR 10.6.0 (`Dockerfile`) | FRR 10.7.0 (`Dockerfile.grout`)        |
| leafkind1, leafkind2, spine  | FRR 10.6.0 (`kind.clab.yml`) | FRR 10.6.0 (`kind.clab.yml`) — unchanged |

### PMSI Tunnel Attribute encoding difference

EVPN Type-3 IMET (Inclusive Multicast Ethernet Tag) routes carry a PMSI Tunnel Attribute (RFC 6514) that specifies the tunnel endpoint for BUM traffic flooding.

- **FRR 10.6.0**: encodes PMSI as **length 9** (5-byte header + 4-byte IPv4 address). For IPv6 VTEPs, it writes `0.0.0.0` (uses `attr->nexthop`, the IPv4-only field). On receive, it **only accepts length 9**.
- **FRR 10.7.0** (with PR #21507): properly encodes IPv6 PMSI as **length 21** (5-byte header + 16-byte IPv6 address). On receive, accepts both length 9 and length 21.

### EVPN route path

```
PE1 (FRR 10.7) → leafkind1 (FRR 10.6) → spine (FRR 10.6) → leafkind2 (FRR 10.6) → PE2 (FRR 10.7)
```

### Kernel datapath (all FRR 10.6.0) — PASSES

1. PE1 generates Type-3 IMET for VNI 310 with PMSI length 9 (tunnel endpoint = `0.0.0.0`)
2. leafkind1 (10.6) accepts it — length 9 is expected
3. Route relays through spine → leafkind2 → PE2
4. PE2 receives the IMET, extracts the actual VTEP address from the **NLRI OriginatingRouterIP** field (`fd00:64::1`), which is correctly IPv6 in both FRR versions
5. `zebra_vxlan_remote_vtep_add` → `DPLANE_OP_VTEP_ADD` → kernel adds remote VTEP to VXLAN flood list
6. ARP broadcast floods to remote PE → ARP resolves → pod-to-pod connectivity works

### Grout datapath (PE=10.7, transit=10.6) — FAILS

1. PE1 generates Type-3 IMET for VNI 310 with PMSI length 21 (tunnel endpoint = `fd00:64::1`, correct per PR #21507)
2. leafkind1 (10.6) **rejects** the IMET — only accepts PMSI length 9, treats length 21 as malformed
3. The Type-3 IMET route is **silently dropped** at the first transit hop
4. PE2 never receives PE1's IMET (and vice versa)
5. `DPLANE_OP_VTEP_ADD` is never called for VNI 310
6. Grout flood VTEP list is empty (`n_flood_vteps == 0`)
7. All BUM traffic dropped: `vxlan_flood_drop: 55 batches/58 packets` (node0), `24/26` (node1)
8. ARP for 192.173.24.3 never resolves → `FAILED` in pod1 ARP table → "Host is unreachable"

### Why IPv4 VTEP (VNI 110) still passes with grout

Both FRR 10.6 and 10.7 encode IPv4 PMSI identically — length 9 with the correct IPv4 tunnel endpoint address. No version mismatch occurs, so the route flows through the transit infrastructure without issue.

## Evidence from CI logs

- **No flood VTEP adds for VNI 310**: grout logs show `grout_vxlan_flood_update_ctx: add 100.65.0.1 vni=110` (IPv4 VNI) but zero entries for VNI 310
- **Flood drops**: `vxlan_flood_drop` counters confirm BUM traffic is discarded due to empty flood list
- **ARP failure**: pod1 dump shows `192.173.24.3 dev net1 FAILED`
- **BGP table**: both PEs generate local Type-3 `[3]:[0]:[128]:[fd00:64::X]` with `RT:64514:310` but neither has the remote PE's Type-3 entry
- **Type-2 routes work**: MAC/IP routes (which don't depend on PMSI) from the same RD/RT ARE exchanged — confirming the BGP sessions and EVPN fabric are functional; only PMSI-dependent Type-3 IMET is affected

## Fix

Align FRR versions across the entire EVPN fabric. Either:

1. **Upgrade transit infrastructure** — change `clab/singlecluster/kind.clab.yml` line 6:
   ```yaml
   image: quay.io/frrouting/frr:10.7.0
   ```

2. **Upgrade kernel datapath image** — change `Dockerfile` line 1:
   ```dockerfile
   ARG FRR_IMAGE=quay.io/frrouting/frr:10.7.0
   ```

Both changes should be applied together so that all FRR instances in the test topology speak the same PMSI encoding.

## Related

- FRR PR [#21507](https://github.com/FRRouting/frr/pull/21507) — "bgpd: Fix PMSI Tunnel Attribute encoding for IPv6 VTEPs"
- Grout dplane plugin: `frr/if_grout.c` lines 104–113 — IPv6 VTEP support gated on `CURRENT_FRR_VERSION >= 10.6`
- Grout dplane plugin: `frr/rt_grout.c` lines 1100–1147 — `grout_vxlan_flood_update_ctx` handles IPv6 correctly via `ipaddr_to_l3_addr`
- Grout VXLAN control: `modules/l2/control/vxlan.c` — `vtep_flood_add` and `vxlan_get_iface` use composite key (VNI, encap_vrf_id)
