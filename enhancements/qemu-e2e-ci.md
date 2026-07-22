# QEMU/KVM E2E CI Lane for SR-IOV Testing

## Summary

The existing kind-based E2E test infrastructure cannot exercise SR-IOV or
DPDK-related features because kind nodes are containers — they have no PCIe
topology, no emulated NICs, and no VF support. This enhancement adds an 
alternative CI lane that boots a full QEMU/KVM virtual
machine with an emulated `igb` SR-IOV NIC, deploys a single-node k3s cluster
inside it, and runs E2E tests against real (emulated) PCIe hardware. This
closes the gap between the kind-based test environment and the bare-metal
environments where openperouter is deployed with grout datapaht.

## Motivation

### Problem

OpenPERouter's grout dataplane can bind SR-IOV Virtual Functions via
DPDK, on both the underlay side and toward the host. Neither of these paths
can be tested in the current CI infrastructure, which is based on clab and kind/

This means that any change touching the SR-IOV path, the grout DPDK port
creation flow, or the VFIO binding logic ships without automated integration
coverage.

### Goals

- Provide a CI lane that can exercise the full SR-IOV lifecycle — VF creation,
  VFIO binding, PCI address discovery — on every PR, triggered by a label.
- Reuse GitHub Actions hosted runners (Ubuntu 24.04 with nested KVM) so that
  no dedicated hardware is required.
- Keep the QEMU environment self-contained and disposable: a single Make
  target boots it, another tears it down.
- Run existing E2E tests unmodified where applicable, and add new
  QEMU-specific tests for hardware-dependent flows (VF setup, grout port
  creation via PCI passthrough).
- Produce actionable CI artifacts on failure: serial console logs, journalctl
  output, k3s/pod logs, PCI device state.

### Non-Goals

- Replacing the kind-based E2E lane. The kind lane remains the default and
  covers the majority of features. The QEMU lane is complementary.
- Full IOMMU emulation. The QEMU VM uses VFIO in `no-IOMMU` mode, which is
  sufficient for functional testing but does not validate IOMMU-based isolation.
- Performance benchmarking. The emulated igb NIC is orders of magnitude slower
  than real hardware; the lane validates correctness, not throughput.

## Proposal

### Architecture

The QEMU lane creates a minimal test topology:

```
┌────────────────────────────┐
│  GitHub Actions runner     │
│                            │
│  ┌──────────────────────┐  │
│  │  QEMU/KVM VM         │  │
│  │  - Fedora Cloud      │  │
│  │  - k3s single-node   │  │
│  │  - igb PF + 2 VFs    │  │
│  │  - openperouter pods  │  │
│  │         │ enp1s0      │  │
│  └─────────┼────────────┘  │
│            │ TAP            │
│     ┌──────┴──────┐         │
│     │ br-underlay │         │
│     └──────┬──────┘         │
│            │ veth           │
│  ┌─────────┴────────────┐  │
│  │  FRR TOR container   │  │
│  │  (AS 65000)          │  │
│  └──────────────────────┘  │
└────────────────────────────┘
```

**VM**: A Fedora Cloud qcow2 image, provisioned via cloud-init, runs k3s as
the single-node Kubernetes distribution. The QEMU machine type is `q35` with a
`pcie-root-port`, which allows the emulated `igb` NIC to expose SR-IOV
capabilities (up to 7 VFs). The VM boots with KVM acceleration on the runner's
host CPU.

**Underlay network**: A Linux bridge (`br-underlay`) on the host connects the
VM's igb NIC (via a TAP device) to an FRR container acting as a mock TOR
switch. The TOR peers with the openperouter instance over BGP (AS 65000 <->
AS 64514) on the `192.168.100.0/24` subnet.

**Management network**: A separate `user`-mode (SLIRP) NIC provides SSH access
(port-forwarded to host port 2222) and k3s API access (port-forwarded to
6443). This keeps management traffic isolated from the underlay.

### VM Provisioning

The VM image is prepared once per CI run:

1. Download a Fedora Cloud base qcow2 image.
2. Resize it to 20 GB to accommodate k3s and container images.
3. Generate a cloud-init ISO that installs k3s (without starting it),
   loads kernel modules (`vfio-pci`), configures hugepages, and injects an
   SSH public key for passwordless access.

Cloud-init runs on first boot; subsequent steps wait for it to complete before
proceeding.

### SR-IOV Setup Inside the VM

OpenPERouter does not manage VF creation or driver binding — it expects
VFs to already exist and be bound to the appropriate driver (see the
[DPDK-accelerated underlay enhancement](grout-dpdk-underlay.md)). In
production this is the responsibility of the operator or the SR-IOV
device plugin. In the QEMU lane, the VM setup scripts simulate what an
operator would do on a bare-metal node:

1. Discover the igb PF by scanning `/sys/class/net/*/device/sriov_totalvfs`.
2. Create 2 VFs by writing to `sriov_numvfs`.
3. Enable VFIO no-IOMMU mode (`enable_unsafe_noiommu_mode=1`) — full IOMMU
   is not available in the emulated environment.
4. Bind one VF to `vfio-pci` so it is ready for grout DPDK port consumption.
5. Unmanage the PF from NetworkManager to prevent address conflicts, then
   assign the underlay IP (`192.168.100.10/24`).

This setup is purely test infrastructure — none of these steps are
performed by the openperouter controller at runtime.

### Test Execution

Tests are selected via Ginkgo label filters:

- `qemu-support`: tests that require the QEMU VM environment (SR-IOV
  hardware, specific NIC naming).
- A `--qemu-mode` flag is passed to the test suite, which tests check at
  runtime to skip when not in a QEMU environment.

The current QEMU-specific tests cover:

- **VF setup verification**: confirms the test infrastructure correctly
  prepared VFs (PF driver is `igb`, a VF is bound to `vfio-pci`) — i.e.
  validates the preconditions that openperouter expects to find at runtime.
- **L3Passthrough with underlay**: creates an Underlay pointing at the igb PF,
  verifies FRR configuration, establishes a BGP session with the mock TOR,
  and configures a host session.
- **GroutPort DPDK underlay** (scaffolded): placeholder tests for the
  future GroutPort API that will exercise DPDK port creation via PCI address
  passthrough.

### CI Workflow Integration

The QEMU lane is a separate job (`qemu-e2etests`) in the CI workflow, gated
behind a PR label (`run-qemu-tests`). This keeps it opt-in:

- It is relatively slow (~10-15 min for VM boot + cloud-init + test
  execution).
- It requires ubuntu-24.04 runners for QEMU 8.2+ igb support.
- Not every PR touches SR-IOV or grout code.

On failure, the job collects logs (serial console, dmesg, journalctl, PCI
state, k3s logs, openperouter pod logs, FRR TOR state) and uploads them as
CI artifacts.

### Relationship to Existing Infrastructure

| Aspect | kind lane | QEMU lane |
|---|---|---|
| Trigger | Every PR (default) | Label-gated (`run-qemu-tests`) |
| Kubernetes | kind (multi-node) | k3s (single-node) |
| Network topology | containerlab | QEMU bridge + FRR container |
| SR-IOV | Not possible | Emulated igb VFs |
| DPDK / grout | TAP-only (test mode) | VFIO-bound VFs available |
| Run time | ~5-8 min | ~10-15 min |

The kind lane continues to cover all non-hardware-dependent features
(EVPN, L3VNI, L2VNI, passthrough, operator, helm, systemd mode). The QEMU
lane specifically targets the SR-IOV and DPDK-dependent paths.

## Test Plan

- **QEMU VF Setup**: verify VF creation, driver binding, and PCI topology
  inside the VM.
- **L3Passthrough over emulated SR-IOV NIC**: deploy underlay + passthrough
  CRDs, verify BGP session establishment with the mock TOR, verify FRR
  configuration.
- **GroutPort (future)**: once the GroutPort API is implemented, test DPDK
  port creation using the VF's PCI address.
- **Failure diagnostics**: verify that log collection captures all relevant
  artifacts when a test fails.

## Future Work

- **Always-on lane**: once the QEMU lane is stable and fast enough, consider
  running it on every PR rather than gating on a label.
- **Multi-node QEMU**: boot multiple VMs to test EVPN multi-homing or
  multi-node L2VNI over emulated SR-IOV.
- **IOMMU emulation**: when QEMU/KVM gains better vIOMMU support on CI
  runners, drop the no-IOMMU workaround to test the full VFIO isolation path.
- **GroutPort API integration**: the scaffolded GroutPort tests become real
  tests once the API lands (see [grout-dpdk-underlay](grout-dpdk-underlay.md)
  and grout-dataplane.md milestone M1a).

## Drawbacks

- **CI cost**: the QEMU lane is slower than kind and uses more resources (KVM,
  disk for the qcow2 image). Keeping it label-gated mitigates this.
- **Emulation fidelity**: the igb emulation in QEMU is functional but not
  hardware-identical. Subtle driver differences between emulated and real
  igb NICs could cause false positives or negatives.
- **Single-node only**: k3s in the VM is a single-node cluster, so tests
  cannot exercise multi-node scheduling or cross-node VXLAN tunnels.

## Implementation History

- 2026-07-17: Initial enhancement document drafted
