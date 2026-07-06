# DPDK-Accelerated Underlay Ports for Grout

## Summary

Replace the TAP-with-`remote=` underlay port mechanism in grout with
direct DPDK port attachment. After the
[grout-dataplane](grout-dataplane.md) enhancement (M1),
`configureUnderlayPort()` creates a `net_tap` PMD with `remote=<nic>`,
which installs TC ingress rules to redirect packets between the underlay
NIC and grout — adding kernel overhead on the underlay fast path. By
binding a network device (PF or VF) directly to a DPDK poll-mode driver
(e.g. `vfio-pci`), grout can send and receive underlay traffic entirely
in user-space, eliminating the kernel data path for VXLAN encap/decap
and BGP-learned forwarding.

The controller takes ownership of driver binding and IP address
migration: it reads the IP configuration from the kernel netdev before
binding the DPDK driver, applies it to the grout port, and restores it
on teardown.

This enhancement extends the `UnderlayInterface` discriminated union
(extended with the `CNIDevice` mode by the
[controller-provisioned-underlay-interfaces](controller-provisioned-underlay-interfaces.md)
enhancement) with a new `GroutDevice` mode.

## Motivation

### Goals

- **Remove the kernel from the underlay fast path.** The current
  TAP+`remote=` approach routes every underlay packet through TC ingress
  rules in the kernel. Direct DPDK port attachment eliminates this
  overhead.
- **Expose device selection in the API.** Users need a way to tell the
  controller which device to bind — by PCI address, PF
  name + VF index, or netlink device name — without external CNI tooling.
- **Automatic IP address migration.** The controller reads the IP
  configuration from the kernel netdev before binding the DPDK driver
  and applies it to the grout port via `grcli`. On teardown the
  original driver and IP addresses are restored.
- **Controller-managed driver binding.** The controller determines the
  NIC type and binds the appropriate driver (`vfio-pci` for Intel,
  namespace move for Mellanox/bifurcated). The user does not need to
  pre-bind devices to a DPDK driver.
- **Preserve the kernel-based TAP path as a fallback.** The existing
  `NetworkDevice` mode remains available for environments without
  DPDK-capable hardware.

### Non-Goals

- **Managing VF creation on the host.** When SR-IOV is used, the user
  creates VFs (`echo N > /sys/.../sriov_numvfs`) before the controller
  consumes them.
- **Workload-facing VF pairs for L2VNI HW acceleration.** Covered by
  milestone M3a in [grout-dataplane](grout-dataplane.md).
- **Replacing CNIDevice mode.** CNIDevice serves kernel-based NIC
  sharing (macvlan/ipvlan). GroutDevice is for DPDK-bound devices
  (PFs or VFs).

## User Stories

#### Story 1: High-Throughput Underlay
As a network operator with DPDK-capable NICs, I want grout to attach
a device (PF or VF) directly via DPDK so that underlay traffic avoids
the kernel entirely and achieves line-rate forwarding.

#### Story 2: Device Selection by PCI Address
As an operator, I want to specify a device's PCI address in the Underlay
spec so the controller binds the correct device — whether it is a PF or
a VF.

#### Story 3: VF Selection by PF + Index
As an operator on a multi-NIC node, I want to specify the PF name and VF
index so that the controller resolves the correct VF without requiring
me to look up PCI addresses.

#### Story 4: Device Selection by Netlink Name
As an operator who pre-configures devices with IP addresses using
standard Linux tools, I want to specify the kernel netlink device name
so the controller picks up the device along with its existing IP
configuration.

## Proposal

### Overview

After the controller-provisioned-underlay-interfaces enhancement, the
`UnderlayInterface` union has two modes: `NetworkDevice` and `CNIDevice`.
This enhancement adds a third: `GroutDevice`.

| Mode | Behavior | IPAM | Datapath |
|------|----------|------|----------|
| `NetworkDevice` | Moves host device; grout creates TAP+`remote=` | Native (CIDR-derived) | TAP PMD (kernel TC redirect) |
| `CNIDevice` | Invokes CNI plugin | Delegated to CNI | Kernel |
| `GroutDevice` | Binds a network device (PF or VF) to grout as a DPDK port | Scraped from kernel netdev | DPDK PMD (user-space) |

`GroutDevice` is only valid when `--datapath=grout`. The controller
rejects it in kernel datapath mode. A webhook validation must be implemented 
for this.

### Device Selection

The operator identifies the target device through three mutually exclusive
field groups on `GroutDevice`:

| Selector | Use case |
|----------|----------|
| `pciAddress` | PCI address of a PF or VF (e.g. `0000:03:02.0`) |
| `pfName`, `vfIndex` | PF name + VF index (e.g. `enp3s0f0` + `2`) |
| `netlinkName` | Kernel netlink device name (e.g. `enp3s0f0v0`) |

All selectors resolve to a PCI address and a netlink device name at
reconcile time via sysfs/netlink. If the netlink device exists and it
has one or more IP address, these are used to configure the Grout port.

### Port Creation Flow

1. **Resolve device** — Resolve the selector to both a PCI address and a
   netlink device name:
   - `pciAddress`: look up the netlink name via
     `/sys/bus/pci/devices/<addr>/net/`.
   - `pfName` + `vfIndex`: read symlink
     `/sys/class/net/<pfName>/device/virtfn<vfIndex>`, extract PCI
     address, then look up netlink name as above.
   - `netlinkName`: look up the PCI address via
     `/sys/class/net/<name>/device` symlink.
2. **Save original state** — Read the IP addresses from the netlink
   device and the current kernel driver name (from
   `/sys/bus/pci/devices/<addr>/driver`). Write both to a per-device
   state file at `/var/run/openperouter/grout/<pci-address>.json`.
3. **Bind DPDK driver** — Determine the NIC type:
   - *Non-bifurcated* (e.g. Intel): unbind from the current kernel driver
     and bind to `vfio-pci`. This destroys the kernel netdev.
   - *Bifurcated* (e.g. Mellanox `mlx5`): move the netlink device into
     the perouter namespace. The kernel driver stays; DPDK shares the
     device via the bifurcated model.
4. **Create grout port** —
   `grcli interface add port u_<name> devargs <pci> [mtu MTU] [rxqs N_RXQ] [qsize Q_SIZE]`
   Options are appended only when set in `portOptions`.
5. **Assign addresses** —
   `grcli address add <cidr> iface u_<name>` for each saved IP address.
6. **Kernel route for FRR** — add connected route on `main` so BGP
   sessions transit grout (same as today).

### API Examples

##### GroutDevice with PCI address

The device at `0000:03:02.0` must already exist as a kernel netdev with
an IP address configured (e.g. via `ip addr add`).

```yaml
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-dpdk
spec:
  asn: 64514
  interfaces:
    - type: GroutDevice
      groutDevice:
        pciAddress: "0000:03:02.0"
  tunnelEndpoint:
    cidrs:
      - "100.65.0.0/24"
  neighbors:
    - address: 192.168.1.1
      asn: 65000
```

##### GroutDevice with PF + VF index (per-node)

```yaml
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-worker-0
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: worker-0
  asn: 64514
  interfaces:
    - type: GroutDevice
      groutDevice:
        pfName: enp3s0f0
        vfIndex: 0
  neighbors:
    - address: 192.168.1.1
      asn: 65000
```

##### GroutDevice with netlink name

```yaml
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-worker-0
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: worker-0
  asn: 64514
  interfaces:
    - type: GroutDevice
      groutDevice:
        netlinkName: enp3s0f0v0
  neighbors:
    - address: 192.168.1.1
      asn: 65000
```

## Design Details

### API Types

```go
// +union
type UnderlayInterface struct {
  // +kubebuilder:validation:Enum=NetworkDevice;CNI;GroutDevice
  // +unionDiscriminator
  Type          string         `json:"type,omitempty"`
  NetworkDevice *NetworkDevice `json:"networkDevice,omitempty"`
  CNIDevice     *CNIDevice     `json:"cniDevice,omitempty"`
  GroutDevice   *GroutDevice   `json:"groutDevice,omitempty"`
}

// GroutDevice specifies a network device (PF or VF) to bind to grout as
// a DPDK port. Exactly one selector must be used: pciAddress,
// pfName + vfIndex, or netlinkName. The device must exist as a kernel
// netdev with at least one IP address configured. The controller reads
// the IP before binding the DPDK driver and restores it on teardown.
// +kubebuilder:validation:XValidation:rule="[has(self.pciAddress), has(self.pfName), has(self.netlinkName)].filter(x, x).size() == 1",message="specify exactly one of pciAddress, pfName+vfIndex, or netlinkName"
// +kubebuilder:validation:XValidation:rule="!has(self.pfName) || has(self.vfIndex)",message="vfIndex is required when pfName is set"
// +kubebuilder:validation:XValidation:rule="!has(self.vfIndex) || has(self.pfName)",message="pfName is required when vfIndex is set"
type GroutDevice struct {
  // +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`
  PCIAddress *string            `json:"pciAddress,omitempty"`
  // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
  // +kubebuilder:validation:MaxLength=15
  PFName      *string           `json:"pfName,omitempty"`
  // +kubebuilder:validation:Minimum=0
  VFIndex     *int              `json:"vfIndex,omitempty"`
  // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
  // +kubebuilder:validation:MaxLength=15
  NetlinkName *string           `json:"netlinkName,omitempty"`
  // portOptions specifies optional DPDK port parameters.
  // +optional
  PortOptions *GroutPortOptions `json:"portOptions,omitempty"`
}

type GroutPortOptions struct {
  // +kubebuilder:validation:Minimum=68
  // +kubebuilder:validation:Maximum=9702
  MTU *int `json:"mtu,omitempty"`
  // +kubebuilder:validation:Minimum=1
  // +kubebuilder:validation:Maximum=64
  RXQueues *int `json:"rxQueues,omitempty"`
  // +kubebuilder:validation:Minimum=64
  // +kubebuilder:validation:Maximum=32768
  QSize *int `json:"qSize,omitempty"`
}
```

### Saved Device State

Before binding the DPDK driver the controller writes a per-device state
file to `/var/run/openperouter/grout/<pci-address>.json`:

```json
{
  "pciAddress": "0000:03:02.0",
  "netlinkName": "enp3s0f0v0",
  "originalDriver": "ice",
  "addresses": ["192.168.1.10/24", "fd00::10/64"]
}
```

This file is read at teardown to restore the original driver and IP
addresses. It is deleted after successful restoration.

### Datapath Validation

`KernelDatapathConfigValidator` is extended to reject `GroutDevice`.

### Device Resolution

Operates on sysfs/netlink. Every selector resolves to both a PCI address
and a netlink device name:

- **PCIAddress**: validate format, check `/sys/bus/pci/devices/<addr>`,
  read netlink name from `/sys/bus/pci/devices/<addr>/net/`.
- **PFName + VFIndex**: read symlink
  `/sys/class/net/<pf>/device/virtfn<idx>`, extract PCI address, then
  look up netlink name as above.
- **NetlinkName**: validate the device exists, read PCI address from
  `/sys/class/net/<name>/device` symlink.

### Teardown

On Underlay deletion or netns rebuild:

1. `grcli interface del u_<name>` — removes the DPDK port.
2. Read the saved state file from
   `/var/run/openperouter/grout/<pci-address>.json`.
3. **Restore driver** — Rebind the device to the original kernel driver
   recorded in the state file (`echo <pci> > /sys/bus/pci/drivers/<orig>/bind`).
   For bifurcated drivers, move the netlink device back to the host
   namespace.
4. **Restore IP addresses** — Re-apply the saved IP addresses to the
   re-created kernel netdev.
5. Delete the state file.

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Device not available (not created, no kernel netdev) | Clear error at reconcile with PCI address and resolution source |
| No IP address configured on the device | The grout port is configured with zero IP addresses |
| `vfio-pci` module not loaded | Check before binding; surface actionable error in status |
| State file lost (e.g. `/var/run` cleared) | Log warning; operator must manually rebind driver and restore IP |
| Driver rebind fails at teardown | Retry with backoff; leave state file for manual recovery |
| Multiple Underlays claim the same device | `grcli interface add` fails with "device busy"; surfaced in status |

### Test Plan

- **E2E tests / Kind**: Against grout in test-mode (no hugepages),
  verify port creation with `net_tap` devargs (no VF hardware in CI),
  address assignment, and teardown.
- **E2E tests / QEMU**: Deploy a cluster based on KVM / QEMU with emulated
  SR-IOV NICs. Running the entire e2etest suite is hard, as the same clab
  topology can't be implemented with VMs. A small set of test cases will be 
  implemented for this lane, using a simple FRR BGP peer in a container.
- **Validation tests**: `GroutDevice` rejected when grout disabled;
  missing VFSelector sub-struct rejected by CEL.

## Alternatives

### Alternative 1: Use CNIDevice with SR-IOV CNI

Use existing `CNIDevice` mode with an `sriov` CNI plugin config.

**Why not chosen:** The SR-IOV CNI moves a kernel netdev into a
container namespace — it does not hand off to grout's DPDK port creation.
IPAM via CNI is meaningless for DPDK-bound interfaces (no kernel netdev
to assign the IP to).

### Alternative 2: Add DPDK devargs to NetworkDevice

Extend `NetworkDeviceConfig` with an optional `devargs` field.

**Why not chosen:** `NetworkDevice` semantics are "move a kernel device."
DPDK port binding is fundamentally different — no kernel device to move.
Mixing both in one type makes validation harder and the API confusing.
No device selector abstraction — the operator must always know PCI addresses.

### Alternative 3: Inline IPAM in the API spec

Carry IP addresses in the `GroutDevice` struct (e.g. a `GroutPortIPAM`
field with an `addresses` list) instead of reading them from the kernel.

**Why not chosen:** The device must have a kernel netdev with an IP
address *before* the controller binds it — reading the IP from the
existing configuration is simpler, avoids duplication between the host
config and the Underlay spec, and guarantees that the address is
restored correctly on teardown.
