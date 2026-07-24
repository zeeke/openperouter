# Plan: Simplified FRR TOR for QEMU E2E Tests

## Context

The current clab/ topology uses a full spine-leaf fabric (spine, 2 EVPN leaves, 1 SRv6 leaf, 2 Kind-facing leaves, multiple host containers) with ~20 test files exercising every combination. The QEMU environment (`hack/qemu/vm/frr/`) has only a minimal FRR TOR (BGP with ipv4 unicast + l2vpn evpn, no VRFs, no ISIS, no SRv6) and only 2 test files (L3Passthrough + GroutPort placeholder).

**Goal**: Upgrade the single FRR TOR container to support all protocol scenarios, then add ~6 new tests for one happy path per resource type. Target: 8 total scenarios (under 10).

## Part 1: Enhanced FRR TOR Container

### 1a. `hack/qemu/vm/frr/frr.conf` — full rewrite

Replace the current minimal config with a comprehensive one:

- **BGP AS 65000** with `remote-as external` (accepts any eBGP peer ASN)
- **All address families** activated for the neighbor: ipv4 unicast, ipv6 unicast, l2vpn evpn, ipv4 vpn, ipv6 vpn
- **ISIS** level-1, NET `49.0001.0000.0000.0001.00`, on `eth0`
- **SRv6** locator `fd00:0:10::/48` (usid, block-len 32, node-len 16)
- **VRF red** (VNI 100): static routes 192.168.20.0/24 + 2001:db8:20::/64, SRv6 VPN export (RD 100.64.0.1:100, RT 65000:100 + 64514:100), EVPN advertise ipv4/ipv6 unicast
- **VRF blue** (VNI 200): static routes 192.168.21.0/24 + 2001:db8:21::/64, same pattern with RD/RT :200

The TOR has all protocols ready simultaneously. The openperouter chooses which address families to negotiate per-test via its Underlay CR.

### 1b. `hack/qemu/vm/frr/daemons` — enable isisd

Add `isisd=yes` and `bfdd=yes` to the daemons file (currently only zebra, bgpd, staticd are enabled).

### 1c. `hack/qemu/vm/start-tor.sh` — add network setup

After creating the container and veth pair, add Linux-level setup inside the container's netns:

1. **IPv6 on eth0**: `2001:db8:100::1/64`
2. **Loopback addresses**: `100.64.0.1/32` (VTEP), `fd00:64::1/128` (IPv6 VTEP), `2001:db8:1234::1/128` (SRv6 source)
3. **VRFs**: `red` and `blue` with `net.vrf.strict_mode=1`
4. **VXLAN interfaces**: `vxlan100` (VNI 100, local 100.64.0.1) and `vxlan200` (VNI 200, local 100.64.0.1)
5. **Bridges**: `br100` (master vrf red, member vxlan100) and `br200` (master vrf blue, member vxlan200)
6. **SRv6 sysctls**: `seg6_enabled=1` on all/eth0, `forwarding=1` on all IPv6

### Addressing Plan

| Resource | Address |
|----------|---------|
| TOR eth0 IPv4 | 192.168.100.1/24 |
| TOR eth0 IPv6 | 2001:db8:100::1/64 |
| TOR lo VTEP IPv4 | 100.64.0.1/32 |
| TOR lo SRv6 source | 2001:db8:1234::1/128 |
| TOR ISIS NET | 49.0001.0000.0000.0001.00 |
| TOR SRv6 locator | fd00:0:10::/48 |
| TOR VRF red static routes | 192.168.20.0/24, 2001:db8:20::/64 |
| TOR VRF blue static routes | 192.168.21.0/24, 2001:db8:21::/64 |
| TOR BGP AS | 65000 |
| VM underlay IP | 192.168.100.10/24 (assigned by setup-vm.sh) |

## Part 2: New E2E Tests

One new file: `e2etests/tests/qemu_scenarios.go`

All tests use **NetworkDevice** with `enp1s0` (GroutPort is already tested by the existing `qemu_l3passthrough.go`). All tests are labeled `QEMUSupport` and skip when `!QEMUMode`.

### Test scenarios

**Describe "QEMU EVPN scenarios" (Ordered)**

BeforeAll: Create standard EVPN underlay (ASN 64514, neighbor 192.168.100.1 AS 65000, TunnelEndpoint 100.65.0.0/24).

1. **"should receive EVPN Type-5 routes via L3VNI"**
   - Create L3VNI "red" (VNI 100, VRF "red") with HostSession
   - Validate BGP session with TOR via `validateSessionWithNeighbor()`
   - Validate Type-5 route for `192.168.20.0/24` received via `frr.EVPNInfo()` / `ContainsType5Prefix()`

2. **"should create L2VNI with L3VNI routing domain"**
   - Create L2VNI (VNI 110) with `routingDomain` -> L3VNI "red", with HostMaster linux-bridge autocreate
   - Validate FRR running config contains the L2VNI bridge/VXLAN configuration

AfterAll: `Updater.CleanAll()`

**Describe "QEMU SRv6 scenarios" (Ordered)**

BeforeAll: Create SRv6 underlay (ASN 64514, neighbor 2001:db8:100::1 AS 65000, TunnelEndpoint 2001:db8:1234:5678::/64, ISIS config, SRv6 locator fd00:0:32::/48).

3. **"should receive L3VPN routes via SRv6"**
   - Create L3VPN "red" (VRF "red", RDAssignedNumber 100, ExportRTs/ImportRTs matching TOR's RT 65000:100)
   - Validate BGP session with TOR
   - Validate FRR running config contains VRF "red" with SRv6 SID
   - Validate VPN routes received via `vtysh -c "show bgp ipv4 vpn json"` (direct executor call, check for 192.168.20.0/24)

4. **"should create L2VNI with L3VPN routing domain"**
   - Create L2VNI (VNI 110) with `routingDomain` -> L3VPN "red", HostMaster linux-bridge autocreate
   - Validate FRR running config contains L2VNI bridge/VXLAN associated with VRF "red"

AfterAll: `Updater.CleanAll()`

**Describe "QEMU RawFRRConfig" (Ordered)**

5. **"should inject raw config into FRR"**
   - Create basic underlay (reuse EVPN underlay pattern)
   - Create RawFRRConfig with `rawConfig: "ip prefix-list QEMU-TEST permit 10.99.0.0/16"`
   - Validate string appears in `frr.RunningConfig()`
   - Delete RawFRRConfig, validate it disappears

AfterAll: `Updater.CleanAll()`

### Existing tests (unchanged)

6. **L3Passthrough** — `qemu_l3passthrough.go` (4 It blocks testing underlay creation, FRR config, BGP session, host session)
7. **GroutPort** — `qemu_groutport.go` (placeholder, tests DPDK underlay)

### Total: 7 scenarios, 5 new + 2 existing

### Key helpers reused (from `e2etests/pkg/`)
- `config.Updater.Update()` / `CleanAll()` — CR lifecycle
- `openperouter.Get()` / `RouterPods()` / `ExecutorForPod()` — router access
- `frr.RunningConfig()` — check FRR config text
- `frr.NeighborInfo()` — check BGP session state
- `frr.EVPNInfo()` + `ContainsType5Prefix()` — check Type-5 routes
- `validateSessionWithNeighbor()` — session establishment with timeout
- `executor.ForContainer("qemu-tor")` — execute commands in TOR container (for vtysh queries on the TOR side if needed)

### No new helper code needed
- L3VPN route verification uses direct `exec.Exec("vtysh", "-c", "show bgp ipv4 vpn json")` + JSON parsing inline — avoids adding `frr.L3VPNInfo()` (which exists only on an unmerged branch)
- ISIS verification uses `frr.RunningConfig()` to check ISIS config is present

## Verification

1. Run `make qemu-setup` to bring up the QEMU VM + enhanced TOR
2. Verify TOR has ISIS adjacency: `docker exec qemu-tor vtysh -c "show isis neighbor"`
3. Verify TOR has SRv6 locator: `docker exec qemu-tor vtysh -c "show segment-routing srv6 locator"`
4. Verify TOR VRFs: `docker exec qemu-tor ip vrf show`
5. Run `make qemu-e2etests` — all QEMU-labeled tests should pass

## Files Modified

| File | Change |
|------|--------|
| `hack/qemu/vm/frr/frr.conf` | Full rewrite: BGP + ISIS + SRv6 + VRFs + VXLAN |
| `hack/qemu/vm/frr/daemons` | Enable isisd, bfdd |
| `hack/qemu/vm/start-tor.sh` | Add IPv6, loopback, VRFs, VXLAN, bridges, SRv6 sysctls |
| `e2etests/tests/qemu_scenarios.go` | **New file**: 5 test scenarios (EVPN, SRv6, RawFRRConfig) |
