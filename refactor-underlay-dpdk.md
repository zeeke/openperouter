# Plan: Align GroutPort IPAM with Enhancement Doc (Kernel-Scraped IPs)

## Context

The enhancement doc (`enhancements/grout-dpdk-underlay.md`) specifies that GroutPort
IPAM should work by **reading IP addresses from the kernel netdev before binding the
DPDK driver**, saving them to a state file, applying them to the grout port, and
restoring them on teardown. The current implementation instead uses **inline IPAM** —
the user manually specifies addresses in the API spec via `ipam.addresses`. This
divergence means:

- Users must duplicate IP configuration between the host and the Underlay CR.
- There is no state file to restore the original driver or IPs on teardown.
- The driver is never restored on teardown (the original driver name isn't recorded).

This plan aligns the implementation with the doc: remove inline IPAM, scrape IPs
from the kernel, persist device state, and restore everything on teardown.

---

## Changes

### 1. Remove inline IPAM from the API

**File:** `api/v1alpha1/underlay_types.go`

- Delete the `GroutPortIPAM` struct (lines 256-269).
- Remove the `IPAM GroutPortIPAM` field from `GroutPortConfig` (line 249) and its
  `+required` marker.
- Regenerate deepcopy (`make generate`).

### 2. Remove `Addresses` from `GroutPortParams`

**File:** `internal/hostnetwork/underlay.go`

- Remove the `Addresses []string` field from `GroutPortParams` (line 62).
  Addresses will be scraped from the kernel at setup time, not carried through
  the conversion layer.
- Clean up the `TODO - not here` comment on `NetlinkDevice`; it now belongs here
  as a proper field.

### 3. Update conversion to stop populating addresses

**File:** `internal/conversion/host_conversion.go`

- In `groutPortInterfaceToHost` (line 614): remove the line that reads
  `iface.GroutPort.IPAM.Addresses` (line 621).

### 4. Implement saved device state

**New file:** `internal/grout/devicestate.go`

Define the state struct and read/write helpers:

```go
type SavedDeviceState struct {
    PCIAddress     string   `json:"pciAddress"`
    NetlinkName    string   `json:"netlinkName"`
    OriginalDriver string   `json:"originalDriver"`
    Addresses      []string `json:"addresses"`
}
```

- `saveDeviceState(state SavedDeviceState) error` — writes JSON to
  `/var/run/openperouter/grout/<pci-address>.json`, creating the directory if needed.
- `loadDeviceState(pciAddr string) (*SavedDeviceState, error)` — reads and unmarshals.
- `deleteDeviceState(pciAddr string) error` — removes the file.

The state dir path should be a package-level var so tests can override it.

### 5. Rework the setup flow to scrape + save + apply

**File:** `internal/grout/underlay.go`

In the `UnderlayInterfaceGroutPort` case of `SetupUnderlay` (lines 83-94), **before**
`prepareGroutPortDriver`:

1. Look up the kernel netlink device name via `sriov.GetPCINetDevice(pciAddr)`.
2. Read IP addresses from the kernel netdev via
   `hostnetwork.AddressesForInterface(netlinkName, hostnetwork.ExcludeLinkLocal())`.
3. Read the current driver via `sriov.GetPCIDriver(pciAddr)`.
4. Save all of the above to the state file via `saveDeviceState(...)`.

Then in `configureGroutPort`:
- Replace `iface.GroutPort.Addresses` with the addresses from the state file
  (pass them as a parameter, or load from the state file using `iface.GroutPort.PCIAddress`).

### 6. Rework teardown to restore driver + IPs

**File:** `internal/grout/underlay.go`

In `RestoreUnderlay`, for the `UnderlayInterfaceGroutPort` case (line 160):

1. Remove grout port addresses and kernel routes (existing logic in
   `removeGroutPortAddresses` — keep).
2. Delete the grout port (existing `client.deletePort` — keep).
3. Load the saved device state via `loadDeviceState(pciAddr)`.
4. If the state has an original driver that is NOT `vfio-pci`:
   - Clear `driver_override` on the PCI device.
   - Write the PCI address to `/sys/bus/pci/drivers/<originalDriver>/bind`.
   - Wait briefly for the kernel netdev to reappear (poll
     `sriov.GetPCINetDevice` with a short retry).
5. Re-apply saved IP addresses to the restored kernel netdev via
   `hostnetwork.AssignIPToInterface`.
6. Delete the state file.

For mlx5 bifurcated teardown: the device is moved back to the host namespace
(existing logic); then re-apply IPs and delete state file.

Add a new `sriov.RestoreDriver(pciAddr, driver string) error` helper in
`internal/sriov/driver.go` that handles the unbind-from-vfio + rebind-to-original
sequence.

### 7. Update e2e tests

**Files:**
- `e2etests/tests/qemu_scenarios.go`
- `e2etests/tests/qemu_l3passthrough.go`

Remove `IPAM: v1alpha1.GroutPortIPAM{...}` from all GroutPort test fixtures.
The QEMU test VMs must have the IP pre-configured on the kernel netdev before
the controller runs (which they likely already do, since the IP needs to exist
for the scraping to work).

### 8. Update QEMU test infrastructure (if needed)

Verify that the QEMU VM setup scripts configure the IP address on the kernel
netdev (e.g. `ip addr add 192.168.100.10/24 dev <vf>`). If not, add this to
the VM init scripts so the controller can scrape it.

---

## Verification

1. **Unit tests**: Add tests for `SavedDeviceState` read/write/delete. Add tests
   for `ResolveNetlinkName`. Mock sysfs via the existing `SysfsRoot` override pattern.
2. **`make generate`**: Regenerate deepcopy after API type changes.
3. **`make manifests`**: Regenerate CRD manifests.
4. **Compile check**: `go build ./...` — ensures all IPAM references are removed.
5. **Existing tests**: `go test ./...` — fix any compilation errors from removed fields.
6. **QEMU e2e**: Run the QEMU test suite to verify end-to-end flow with scraped IPs.
