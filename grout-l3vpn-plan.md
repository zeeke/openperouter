# Grout Datapath: L3VPN Implementation

## Context

The kernel datapath (`internal/hostnetwork/l3vpn.go:SetupL3VPN`) already implements L3VPN support: it creates a kernel VRF (with SRv6 sysctls) and optionally a veth pair for host connectivity. The grout datapath has implementations for L3VNI, L2VNI, underlay, and passthrough, but **L3VPN is entirely missing** — the grout configurator (`grout_config.go:54`) omits `L3VPNs` from the API config, so they're silently ignored when running with `--datapath=grout`.

The goal is to implement the grout equivalent of `SetupL3VPN`, following the established patterns from grout L3VNI (`internal/grout/l3vni.go`).

## Key Design Decisions

L3VPN is SRv6-based (not VXLAN). Unlike L3VNI which creates a VXLAN tunnel interface, L3VPN only needs:
1. A **grout VRF** (via `client.ensureVRF`) — no VXLAN, no bridge
2. A **TAP port** for host connectivity (if `LinkIPs` is set), using `ensureTapPortInHostNamespace`
3. **SRv6Overhead** (64 bytes) for MTU calculation instead of VXLanOverhead

TAP naming will use the SRv6 infix to match the kernel convention: `host-s-<RDAssignedNumber>` / `pe-s-<RDAssignedNumber>`.

## Changes

### 1. New file: `internal/grout/l3vpn.go`

Modeled after `internal/grout/l3vni.go`, but simpler (no VXLAN/bridge):

- **`SetupL3VPN(ctx, client, params hostnetwork.L3VPNParams) error`**
  1. `client.ensureVRF(ctx, params.VRF)` — create the grout VRF
  2. If `params.LinkIPs == nil`, return early (no host connectivity needed)
  3. Compute link pair names: `host-s-<RDAssignedNumber>` / `pe-s-<RDAssignedNumber>`
  4. `ensureTapPortInHostNamespace(ctx, client, peSide, hostSide, params.VRF, ns)` — create TAP port in the VRF, move host TAP to host namespace
  5. `hostnetwork.AssignIPsToInterface(hostTap, linkIPs.HostIPv4, linkIPs.HostIPv6)` — assign IPs to host TAP
  6. `hostnetwork.SetVethMTUForTunnelOverhead(hostTap, underlayMTU, hostnetwork.SRv6Overhead)` — set MTU accounting for SRv6 overhead
  7. `ensurePortAddresses(ctx, client, peSide, linkIPs.NSIPv4, linkIPs.NSIPv6)` — assign IPs to grout port

- **`RemoveAllL3VPNs(ctx, client, targetNS) error`** — delegates to `RemoveNonConfiguredL3VPNs` with empty params

- **`RemoveNonConfiguredL3VPNs(ctx, client, targetNS, configured []hostnetwork.L3VPNParams) error`**
  1. Build set of configured `RDAssignedNumber` values
  2. `client.listInterfaces(ctx)` — list all grout interfaces
  3. `findStaleL3VPNs(ifaces, configuredRDs)` — find grout ports matching `pe-s-%d` that aren't configured
  4. For each stale RD: delete the grout port (`pe-s-<N>`) and host TAP (`host-s-<N>`)

- **`linkPairNamesFromL3VPN(rdAssignedNumber int32) hostnetwork.VethNames`** — returns `host-s-<N>` / `pe-s-<N>`

- **`findStaleL3VPNs(ifaces []groutInterface, configuredRDs map[int32]bool) []int32`** — scans grout interfaces for `pe-s-%d` names not in configured set

- **`removeL3VPN(ctx, client, rdAssignedNumber) error`** — deletes grout port and host TAP for a single L3VPN

### 2. Modify: `internal/controller/routerconfiguration/grout_config.go`

Mirror what `host_config.go` does for L3VPNs:

- **`Configure()`** — add `L3VPNs: config.L3VPNs` to the `apiConfig` struct (line 51-56)
- Add L3VPN setup loop (after L3VNI loop, before L2VNI loop — same position as kernel):
  ```
  for _, l3vpn := range hostConfig.L3VPNs {
      grout.SetupL3VPN(ctx, groutClient, l3vpn)
      // track in configuredL3VPNs, failedL3Domains on error
  }
  ```
- Add `configuredL3VPNs` VRF names to `configuredVRFs` map
- Add cleanup call: `grout.RemoveNonConfiguredL3VPNs(ctx, groutClient, targetNS, configuredL3VPNs)`
- **`restoreUnderlayGrout()`** — add `grout.RemoveAllL3VPNs(ctx, groutClient, targetNS)` before `RemoveAllVRFs`

## Verification

- `go build ./...` — compile check
- `go test ./internal/grout/...` — run existing grout tests
- `go test ./internal/controller/routerconfiguration/...` — run configurator tests
- `go vet ./...` and `golangci-lint run` if available
