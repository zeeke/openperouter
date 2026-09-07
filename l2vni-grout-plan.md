# L2VNI Support for Grout — Implementation Plan

## Context

OpenPERouter is a Kubernetes-native Provider Edge router. It terminates VPN
protocols (EVPN/VXLAN) on cluster nodes using FRR for BGP and either the Linux
kernel or grout (a DPDK-based dataplane) for packet forwarding.

L2VNI (Layer 2 VNI) extends L2 Ethernet domains across VXLAN tunnels, enabling
EVPN Type 2 (MAC/IP) route advertisement. It differs from L3VNI in that traffic
is bridged (L2 switching) rather than routed (L3 VRF).

L2VNI is fully implemented for the kernel datapath but explicitly blocked for
grout. The block is in `internal/conversion/validate_grout.go:22` —
`ValidateGroutL2VNI()` unconditionally returns an error, and the admission
webhook (`internal/webhooks/l2vni_webhook.go`) rejects L2VNI creation when grout
is enabled.

## Reference: How L3VNI Works in Grout (follow this pattern)

The L3VNI grout implementation (`internal/grout/l3vni.go`) is the template:

1. `setupVNI()` creates a VRF + VXLAN interface via `grcli` commands
   (`grout_client.go:ensureVRF`, `ensureVXLAN`).
2. `ensureTapPortInHostNamespace()` creates a TAP port in grout and moves it to
   the host namespace — this TAP replaces the kernel veth pair.
3. IPs are assigned to the host-side TAP via netlink and to the grout port via
   `ensurePortAddresses()`.
4. Cleanup: `RemoveNonConfiguredVNIs()` deletes stale VXLAN interfaces and VRFs.

The configurator is wired in `internal/controller/routerconfiguration/grout_config.go`:
`GroutDatapathConfigurator.Configure()` loops over L3VNIs and calls
`grout.SetupL3VNI()`, then cleans up stale ones.

## Reference: How L2VNI Works in the Kernel Datapath

The kernel L2VNI implementation (`internal/hostnetwork/vni.go:SetupL2VNI`) does:

1. `setupVNI()` creates VRF (optional — L2VNI VRF can be empty for standalone
   L2), bridge (always), and VXLAN interface inside the PE namespace.
2. Creates a veth pair; one leg stays in the PE namespace enslaved to the bridge,
   the other is exposed to the host namespace.
3. If `HostMaster` is configured, the host-side veth is enslaved to a host-side
   bridge (linux-bridge or OVS bridge) via `setupHostMaster()`.
4. If `L2GatewayIPs` are set, they are assigned to the PE-namespace bridge with
   a fixed MAC address for distributed anycast gateway.

The configurator loop is in `internal/controller/routerconfiguration/host_config.go:117-146`:
it skips L2VNIs whose parent L3 domain failed, calls `hostnetwork.SetupL2VNI()`,
then starts bridge refresh.

## Changes Required

### 1. Remove the validation block

**File:** `internal/conversion/validate_grout.go`

Change `ValidateGroutL2VNI` to return `nil` (like `ValidateGroutL3VNI` does).
Consider adding field-level checks if any HostMaster types are unsupported with
grout.

### 2. Implement `grout.SetupL2VNI()`

**New file:** `internal/grout/l2vni.go`

Mirror the kernel `SetupL2VNI` using grout primitives:

- `setupVNI()` creates VRF (when non-empty) + VXLAN via `grcli` — this function
  already exists in `internal/grout/l3vni.go` and handles both cases. Note: for
  L2VNI the VRF is optional (can be empty string for standalone L2).
- Grout supports bridges natively — use `grcli` to create a bridge and attach
  the VXLAN interface to it.
- Create a TAP port for the host-side interface (same `ensureTapPortInHostNamespace`
  pattern as L3VNI).
- **HostMaster attachment:** The host-side bridge (linux-bridge or OVS) is a
  kernel-level construct, not a grout one. The existing `setupHostMaster()`
  function in `internal/hostnetwork/vni.go:263` operates on a `netlink.Link` and
  can be reused directly — just pass it the grout TAP device instead of a veth.
  This may require exporting `setupHostMaster` or extracting it to a shared
  package.
- **L2GatewayIPs:** Assign to the bridge with a fixed MAC address for distributed
  gateway (same logic as kernel path in `setupL2VNIRouterSide`).

### 3. Wire L2VNI into `GroutDatapathConfigurator.Configure()`

**File:** `internal/controller/routerconfiguration/grout_config.go`

Currently lines 89-98 only loop over L3VNIs. Add:

- An L2VNI loop calling `grout.SetupL2VNI()`, placed after the L3VNI loop.
- Track failed L3 domains (`failedL3Domains` set) so dependent L2VNIs are skipped
  — same pattern as `KernelDatapathConfigurator` in `host_config.go:117-146`.
- Update `grout.RemoveNonConfiguredVNIs()` to accept and track both L3VNI and
  L2VNI params, so stale L2 interfaces get cleaned up too.

### 4. FRR config — no changes needed

L2VNIs don't generate separate FRR config blocks. The VXLAN interface in the PE
namespace triggers FRR to auto-advertise Type 2 EVPN routes. L2GatewayIPs are
already advertised via the parent L3VNI in `internal/conversion/frr_conversion.go`.

### 5. Tests

- Update `grout_config.go` tests if any exist.
- E2E tests with grout flavor + L2VNI (similar to existing
  `e2etests/tests/evpn_l2.go` but with grout enabled).

## Key Files Reference

| Purpose | File |
|---------|------|
| L2VNI API types | `api/v1alpha1/l2vni_types.go` |
| L2VNI host params | `internal/hostnetwork/vni.go` (types `L2VNIParams`, `HostMaster`) |
| Kernel L2VNI setup | `internal/hostnetwork/vni.go` (`SetupL2VNI`, `setupHostMaster`) |
| Grout L3VNI setup (template) | `internal/grout/l3vni.go` |
| Grout client (grcli wrapper) | `internal/grout/grout_client.go` |
| Grout validation block | `internal/conversion/validate_grout.go` |
| Grout configurator | `internal/controller/routerconfiguration/grout_config.go` |
| Kernel configurator (reference) | `internal/controller/routerconfiguration/host_config.go` |
| FRR conversion (L2GW IPs) | `internal/conversion/frr_conversion.go` |
| Host conversion (API to params) | `internal/conversion/host_conversion.go` |
| L2VNI webhook | `internal/webhooks/l2vni_webhook.go` |
| E2E L2 tests | `e2etests/tests/evpn_l2.go` |
