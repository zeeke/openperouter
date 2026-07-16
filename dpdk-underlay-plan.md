# GroutPort: DPDK-Accelerated Underlay Ports

## Context

The grout dataplane currently creates underlay ports using a TAP+`remote=` mechanism: the controller moves a kernel NIC into the perouter netns, then grout creates a `net_tap` PMD with TC ingress rules to redirect packets. This adds kernel overhead on the underlay fast path.

The [design doc (PR #549)](https://github.com/openperouter/openperouter/pull/549) proposes a new `GroutPort` mode for the `UnderlayInterface` union that binds an SR-IOV VF directly to grout via DPDK, eliminating the kernel from the underlay data path. The operator identifies the VF by PCI address or PF name + VF index; IPAM is inline (no CNI).

We build on the current `grout-dev` branch which only has `NetworkDevice`. PR #543 (CNIDevice) is independent — merge conflicts will be small and mechanical.

## Implementation Steps

### 1. API types — `api/v1alpha1/underlay_types.go`

Add `GroutPort` to the `UnderlayInterfaceType` enum and extend the `UnderlayInterface` union:

- Add const `UnderlayInterfaceTypeGroutPort UnderlayInterfaceType = "GroutPort"`
- Update the `+kubebuilder:validation:Enum` marker to `Enum=NetworkDevice;GroutPort`
- Add `GroutPort *GroutPortConfig` field to `UnderlayInterface`
- Add a CEL validation rule for the new arm: `has(self.groutPort) == (self.type == 'GroutPort')`
- Define new types:

```go
// GroutPortConfig — VF selector (pciAddress XOR pfName+vfIndex), inline IPAM, optional port options.
// CEL: exactly one selector group must be set.
type GroutPortConfig struct {
    PCIAddress  *string           `json:"pciAddress,omitempty"`
    PFName      *string           `json:"pfName,omitempty"`
    VFIndex     *int              `json:"vfIndex,omitempty"`
    IPAM        GroutPortIPAM     `json:"ipam"`
    PortOptions *GroutPortOptions `json:"portOptions,omitempty"`
}

type GroutPortIPAM struct {
    Addresses []string `json:"addresses"`  // 1-2 CIDRs, at most one per family
}

type GroutPortOptions struct {
    MTU      *int `json:"mtu,omitempty"`
    RXQueues *int `json:"rxQueues,omitempty"`
    QSize    *int `json:"qSize,omitempty"`
}
```

CEL validations on `GroutPortConfig` (as per design doc):
- Exactly one selector: `has(self.pciAddress) != (has(self.pfName) && has(self.vfIndex))`
- `pfName` requires `vfIndex` and vice versa
- PCI address pattern: `^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`

CEL validations on `GroutPortIPAM.Addresses`:
- MinItems=1, MaxItems=2
- All entries must be valid CIDRs
- At most one IPv4 and one IPv6

Run `make generate manifests` to regenerate deepcopy and CRD YAML.

### 2. VF resolution — new `internal/sysfs/sysfs.go`

Create a sysfs resolver:

```go
func ResolvePCIAddress(selector GroutPortSelector) (string, error)
```

- **PCIAddress path**: validate format, check `/sys/bus/pci/devices/<addr>` exists.
- **PFName+VFIndex path**: read symlink at `/sys/class/net/<pfName>/device/virtfn<vfIndex>`, extract PCI address from the symlink target's basename.

Wrap sysfs reads behind a `var readSysfs = os.ReadLink` for testability.

### 3. Grout client — extend `ensurePort` in `internal/grout/grout_client.go`

The design doc specifies: `grcli interface add port u_<name> devargs <pci> [mtu MTU] [rxqs N_RXQ] [qsize Q_SIZE]`

Add a new method (or extend `ensurePort`) to accept optional port options:

```go
func (c *Client) ensurePortWithOptions(ctx, name, devargs string, opts *PortOptions) error
```

Where `PortOptions` has `MTU`, `RXQueues`, `QSize` — each appended as extra args when non-nil. The existing `ensurePort` can delegate to this with nil options to avoid changing callers.

### 4. Conversion layer — `internal/conversion/host_conversion.go`

The `UnderlayParams` struct (`internal/hostnetwork/underlay.go`) currently carries `UnderlayInterfaces []string` — a flat list of kernel NIC names. GroutPort interfaces are not kernel NICs; they carry PCI info and inline IPAM.

Add a new field to `UnderlayParams`:

```go
type GroutPortParams struct {
    Name        string   // port name suffix (derived from PCI or pfName)
    PCIAddress  string   // resolved PCI address
    Addresses   []string // inline IPAM CIDRs
    PortOptions *GroutPortOptions
}

type UnderlayParams struct {
    UnderlayInterfaces []string          // NetworkDevice names (existing)
    GroutPorts         []GroutPortParams // GroutPort entries (new)
    TargetNS           string
    TunnelEndpoint     *UnderlayTunnelEndpointParams
}
```

In `APItoHostConfig()`, after calling `underlayNetworkDeviceInterfaceNames()`, also extract GroutPort entries and populate `GroutPorts`. For the port name, derive it from the PCI address (e.g., replace `:` and `.` with `_`) to keep it deterministic and unique.

The VF resolution (sysfs readlink for pfName+vfIndex) should happen at reconcile time in the controller, not in the conversion layer — the conversion layer runs in the webhook too, where sysfs isn't available. Pass through the raw selector and resolve in the grout setup path.

**Revised approach**: `GroutPortParams` stores the raw `GroutPortConfig` from the API. VF resolution happens in `grout.SetupUnderlay()`.

### 5. Grout underlay setup — `internal/grout/underlay.go`

Extend `SetupUnderlay()` to handle `GroutPorts` alongside `UnderlayInterfaces`:

```
for each GroutPort in params.GroutPorts:
    1. Resolve PCI address (via sysfs)
    2. For bifurcated drivers (mlx5): move VF netlink to perouter netns
    3. Create grout port: client.ensurePortWithOptions(ctx, "u_"+name, pciAddr, opts)
    4. Assign inline IPAM addresses: client.ensureAddress(ctx, portName, cidr)
    5. Add kernel subnet routes via "main" (same as existing configureUnderlayPort)
    6. Disable rp_filter on the grout-created kernel interface
```

Create a `configureGroutPort()` function parallel to the existing `configureUnderlayPort()`.

Extend `RestoreUnderlay()`: for GroutPort-created ports, addresses are deleted from grout and the port is removed. No kernel NIC migration back (VF stays where the driver puts it).

Extend `UnderlayInterfaces()` and `UnderlayInterfacesToRemove()` to track GroutPort entries too — they all use the `u_` prefix, so existing logic largely works.

### 6. Validation — `internal/conversion/validate_grout.go`

Extend `ValidateGroutUnderlay()`:
- For `GroutPort` interfaces, no IFNAMSIZ check on PCI address (port name is derived differently).
- Validate that the derived port name (`u_` + sanitized PCI) fits in IFNAMSIZ.

### 7. Validation — `internal/conversion/validate_datapath.go`

Extend `KernelDatapathConfigValidator.Validate()` to reject `GroutPort` type interfaces:
```go
func (k *KernelDatapathConfigValidator) Validate(apiConfig APIConfigData) error {
    for _, u := range apiConfig.Underlays {
        for _, iface := range u.Spec.Interfaces {
            if iface.Type == UnderlayInterfaceTypeGroutPort {
                return fmt.Errorf("GroutPort interfaces require --grout-enabled=true")
            }
        }
    }
    return nil
}
```

### 8. Validation — `internal/conversion/validate_underlay.go`

`validateUnderlay()` at line 70 calls `underlayNetworkDeviceInterfaceNames()` and checks for duplicate names. Extend to also check for duplicate GroutPort selectors (duplicate PCI addresses, or duplicate pfName+vfIndex combos).

### 9. Webhooks — `internal/webhooks/underlay_webhook.go`

No direct changes needed — the webhook already delegates to `DatapathConfigValidator.Validate()` which calls `ValidateGroutUnderlay()`. The kernel validator will reject GroutPort when grout is disabled.

### 10. CRD schema tests — `internal/crdschema/crdschema_test.go`

Add test cases following existing patterns:
- **Valid**: GroutPort with pciAddress + IPAM addresses
- **Valid**: GroutPort with pfName + vfIndex + IPAM addresses
- **Valid**: GroutPort with portOptions (mtu, rxQueues, qSize)
- **Invalid**: GroutPort with both pciAddress AND pfName+vfIndex (CEL rejects)
- **Invalid**: GroutPort with pfName but no vfIndex
- **Invalid**: GroutPort with empty addresses
- **Invalid**: GroutPort with 3 addresses (exceeds MaxItems=2)
- **Invalid**: GroutPort with two IPv4 addresses

### 11. Unit tests

- `internal/sysfs/sysfs_test.go`: Test PCI address validation and pfName+vfIndex symlink resolution (mock sysfs reads).
- `internal/grout/underlay_test.go`: Test `configureGroutPort()` with mocked grcli calls.
- `internal/conversion/validate_grout_test.go`: Test GroutPort validation (IFNAMSIZ, kernel-mode rejection).
- `internal/conversion/validate_underlay_test.go`: Test duplicate GroutPort selector detection.
- `internal/conversion/host_conversion_test.go`: Test GroutPort extraction in `APItoHostConfig()`.

### 12. E2E tests

- **Kind/test-mode**: GroutPort with `net_tap` devargs (no real VF in CI). Verify port creation, address assignment, teardown. Add to `e2etests/pkg/infra/underlay.go` and `e2etests/tests/`.
- **Webhook e2e**: GroutPort rejected when grout disabled; CEL validation for malformed selectors.
- The QEMU-based lane with emulated SR-IOV is out of scope for this initial implementation; tracked as follow-up.

### 13. Docs and examples

- Update `API-DOCS.md` and `website/content/docs/configuration/_index.md` with GroutPort.
- Add example YAML under `examples/evpn/groutport-underlay/`.
- Update `enhancements/grout-dpdk-underlay.md` status if needed.

## Files to modify (key ones)

| File | Change |
|------|--------|
| `api/v1alpha1/underlay_types.go` | Add GroutPort types, enum, CEL rules |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated (`make generate`) |
| `config/crd/bases/...underlays.yaml` | Regenerated (`make manifests`) |
| `internal/grout/sysfs.go` | **New** — VF PCI resolution via sysfs |
| `internal/grout/grout_client.go` | Add `ensurePortWithOptions()` |
| `internal/grout/underlay.go` | Add `configureGroutPort()`, extend Setup/Restore |
| `internal/hostnetwork/underlay.go` | Extend `UnderlayParams` with `GroutPorts` |
| `internal/conversion/host_conversion.go` | Extract GroutPort entries in `APItoHostConfig()` |
| `internal/conversion/validate_grout.go` | GroutPort validation |
| `internal/conversion/validate_datapath.go` | Kernel validator rejects GroutPort |
| `internal/conversion/validate_underlay.go` | Duplicate GroutPort selector check |
| `internal/crdschema/crdschema_test.go` | CEL validation test cases |
| Various `*_test.go` files | Unit tests |
| `e2etests/` | E2E test for GroutPort (test-mode) |

## Verification

1. `make generate manifests` — deepcopy + CRD YAML regenerated
2. `make test` — all unit tests pass
3. `go vet ./...` — no issues
4. CRD schema tests validate CEL rules for GroutPort
5. Kernel-mode webhook rejects GroutPort
6. E2E in Kind with grout test-mode: create Underlay with GroutPort, verify port creation + teardown
