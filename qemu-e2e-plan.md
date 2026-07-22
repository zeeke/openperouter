# QEMU/KVM E2E CI Lane for SR-IOV Testing

## Context

The existing e2e CI runs on Kind clusters wired together with containerlab. This
works well for the kernel and grout-in-test-mode datapaths, but cannot test real
SR-IOV VF passthrough — Kind nodes are containers, not VMs, and there is no PCI
bus to emulate VFs on.

The GroutPort feature (DPDK-accelerated underlay via SR-IOV VFs) needs an e2e
lane that runs on an actual VM with emulated SR-IOV hardware. QEMU/KVM supports
this via the `igb` NIC model, which exposes a PCI device that supports VFs in
the guest. The VM runs a single-node K8s cluster (k3s), connected over a Linux
bridge to an FRR container that acts as a TOR switch mock.

This plan targets the `main` branch and is independent of the GroutPort
implementation itself — it builds the VM infrastructure first so the GroutPort
tests have somewhere to run.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  CI Runner (GitHub Actions / bare-metal)                │
│                                                         │
│  ┌──────────────────────┐    ┌───────────────────────┐  │
│  │   QEMU/KVM VM        │    │  FRR container (TOR)  │  │
│  │                      │    │  ASN 65000            │  │
│  │  - k3s single-node   │    │  BGP peer             │  │
│  │  - openperouter      │    │  192.168.100.1/24     │  │
│  │  - grout + DPDK      │    │                       │  │
│  │  - igb VFs on PCI    │    └──────┬────────────────┘  │
│  │                      │           │                   │
│  │  eth0: mgmt (NAT)    │           │                   │
│  │  PF (igb): underlay  │           │                   │
│  └──────┬───────────────┘           │                   │
│         │                           │                   │
│         └───────── br-underlay ─────┘                   │
│                (Linux bridge)                           │
└─────────────────────────────────────────────────────────┘
```

The VM gets two NICs:
- **eth0** — virtio, NAT/user networking for management (SSH, kubectl, image pulls)
- **igb PF** — connected to `br-underlay` bridge, shared with the FRR container

Inside the VM, the igb PF creates VFs via sysfs. The grout GroutPort binds the
VF and peers with the FRR container over BGP.

## Implementation Steps

### 1. VM image build — `hack/qemu-image/`

Create a script and/or Packer/virt-install config that builds a minimal VM image
(qcow2) with:

- **Base OS**: Fedora Cloud (or CentOS Stream 10, matching `Dockerfile.grout`)
- **Packages**: k3s, container runtime (containerd/cri-o), kubectl, iproute2,
  DPDK dependencies (numactl, pciutils), VFIO kernel modules
- **Hugepages**: Configure 512 x 2MB hugepages in grub/kernel cmdline
  (`hugepagesz=2M hugepages=512`)
- **IOMMU**: Enable in kernel cmdline (`intel_iommu=on iommu=pt`)
- **VFIO**: Load `vfio-pci` module at boot
- **k3s**: Pre-installed but not started (started by setup script after boot)

Build approach options (in decreasing preference):
1. **cloud-init on a generic cloud image** — fastest, no custom build. Download
   Fedora Cloud qcow2, pass a cloud-init ISO with packages + config.
2. **Packer** — reproducible but adds a build dependency.
3. **virt-install + kickstart** — more complex.

Recommended: **cloud-init**. The setup script downloads the base cloud image
once (cached in CI), prepares a cloud-init ISO with the package list and kernel
params, and boots the VM. First boot installs packages and reboots with the new
kernel cmdline.

Files:
- `hack/qemu-image/cloud-init/user-data` — cloud-init user-data (packages,
  kernel cmdline, hugepages, VFIO module, k3s install)
- `hack/qemu-image/cloud-init/meta-data` — instance metadata
- `hack/qemu-image/prepare-image.sh` — downloads base image, creates cloud-init
  ISO, optionally pre-bakes the image with packages for CI caching

### 2. VM launch script — `hack/qemu-vm/`

A script that:

1. Creates the `br-underlay` Linux bridge on the host
2. Creates a TAP device for the VM's underlay NIC
3. Launches QEMU with:
   ```
   qemu-system-x86_64 \
     -enable-kvm -cpu host -smp 4 -m 4096 \
     -drive file=<image.qcow2>,if=virtio \
     -cdrom <cloud-init.iso> \
     -netdev user,id=mgmt,hostfwd=tcp::2222-:22,hostfwd=tcp::6443-:6443 \
     -device virtio-net-pci,netdev=mgmt \
     -netdev tap,id=underlay,ifname=vm-underlay,script=no,downscript=no \
     -device igb,netdev=underlay,mac=52:54:00:ab:cd:01 \
     -nographic -serial mon:stdio
   ```
4. Waits for VM to be SSH-reachable
5. Copies the openperouter container image into the VM (via `scp` + `ctr import`
   or `k3s ctr images import`)

Files:
- `hack/qemu-vm/launch-vm.sh` — main launcher
- `hack/qemu-vm/stop-vm.sh` — cleanup (kill QEMU, remove bridge/tap)

### 3. FRR TOR mock — `clab/qemu/`

A minimal FRR container connected to `br-underlay`, acting as the TOR switch.
This is similar to the existing `leafkind1`/`leafkind2` containers but simpler
(single peer, no spine, no VRFs initially).

FRR config:
```
router bgp 65000
 no bgp ebgp-requires-policy
 no bgp network import-check
 no bgp default ipv4-unicast
 neighbor 192.168.100.10 remote-as 64514
 address-family ipv4 unicast
  neighbor 192.168.100.10 activate
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.100.10 activate
  advertise-all-vni
 exit-address-family
exit
```

The TOR runs directly via `docker run` (or podman) on the CI host — no
containerlab needed for this lane.

Files:
- `hack/qemu-vm/frr/frr.conf` — FRR config for TOR mock
- `hack/qemu-vm/frr/daemons` — copy of `clab/frrcommon/daemons`
- `hack/qemu-vm/start-tor.sh` — starts the FRR container attached to
  `br-underlay`

### 4. In-VM setup script — `hack/qemu-vm/setup-vm.sh`

Runs inside the VM (via SSH) after boot to:

1. Create VFs on the igb PF:
   ```
   echo 2 > /sys/class/net/<pf>/device/sriov_numvfs
   ```
2. Bind VF(s) to `vfio-pci`:
   ```
   echo <vf_pci_addr> > /sys/bus/pci/devices/<vf_pci_addr>/driver/unbind
   echo vfio-pci > /sys/bus/pci/devices/<vf_pci_addr>/driver_override
   echo <vf_pci_addr> > /sys/bus/pci/drivers/vfio-pci/bind
   ```
   (For bifurcated drivers like mlx5, the VF stays kernel-bound and grout
   handles it directly — but igb is a split driver, so VFIO binding is needed.)
3. Start k3s:
   ```
   k3s server --write-kubeconfig-mode 644 &
   ```
4. Wait for k3s to be ready
5. Import the openperouter container image
6. Deploy openperouter with grout dataplane (`kustomize` overlay or Helm with
   `datapath=grout`)

### 5. Makefile targets

Add to the main `Makefile`:

```makefile
##@ QEMU E2E

QEMU_VM_IMAGE ?= hack/qemu-vm/fedora-cloud.qcow2
QEMU_SSH_PORT ?= 2222
QEMU_K8S_PORT ?= 6443

.PHONY: qemu-image
qemu-image: ## Prepare the QEMU VM image with cloud-init
	hack/qemu-image/prepare-image.sh

.PHONY: qemu-setup
qemu-setup: qemu-image ## Launch the QEMU VM, FRR TOR, and bridge
	hack/qemu-vm/launch-vm.sh
	hack/qemu-vm/start-tor.sh

.PHONY: qemu-deploy
qemu-deploy: qemu-setup ## Deploy openperouter with grout inside the VM
	hack/qemu-vm/setup-vm.sh

.PHONY: qemu-e2etests
qemu-e2etests: ## Run QEMU e2e tests (VM must be running)
	KUBECONFIG=hack/qemu-vm/kubeconfig \
	  $(GINKGO) -v $(GINKGO_ARGS) --timeout=1h \
	  ./e2etests/suite -- \
	  --kubectl=$(KUBECTL) --groutmode --qemu-mode \
	  $(TEST_ARGS)

.PHONY: qemu-clean
qemu-clean: ## Tear down the QEMU VM and FRR TOR
	hack/qemu-vm/stop-vm.sh
```

### 6. E2E test flag and label — `e2etests/`

Add a new test flag and Ginkgo label:

In `e2etests/tests/common.go`:
```go
QEMUMode bool
```

In `e2etests/suite/suite_test.go`:
```go
flag.BoolVar(&tests.QEMUMode, "qemu-mode", false,
    "running on a QEMU VM with SR-IOV hardware")
```

Add a Ginkgo label:
```go
var QEMUSupport = ginkgo.Label("qemu-support")
```

### 7. Initial QEMU e2e test — `e2etests/tests/qemu_groutport.go`

A small set of test cases that exercise the GroutPort flow end-to-end. These
tests are gated on `QEMUMode` and labeled `qemu-support`:

```go
var _ = ginkgo.Describe("GroutPort DPDK underlay", QEMUSupport, Ordered, func() {
    ginkgo.BeforeAll(func() {
        if !QEMUMode {
            ginkgo.Skip("QEMU mode not enabled")
        }
    })

    ginkgo.It("should create an underlay with GroutPort (PCI address)", ...)
    ginkgo.It("should establish BGP session with TOR via DPDK port", ...)
    ginkgo.It("should assign inline IPAM addresses to grout port", ...)
    ginkgo.It("should tear down GroutPort on underlay deletion", ...)
})
```

These tests create an Underlay CR with `type: GroutPort` pointing at the VF's
PCI address, then verify:
- The grout port exists (`grcli interface show`)
- Addresses are assigned (`grcli address show`)
- BGP session is established with the FRR TOR
- Teardown removes the port

### 8. QEMU test infra helpers — `e2etests/pkg/infra/`

Add QEMU-specific underlay fixtures:

In `e2etests/pkg/infra/underlay.go` (or a new `underlay_qemu.go`):
```go
func QEMUGroutPortUnderlay(pciAddress string) v1alpha1.Underlay {
    return v1alpha1.Underlay{
        Spec: v1alpha1.UnderlaySpec{
            ASN: 64514,
            Interfaces: []v1alpha1.UnderlayInterface{{
                Type: v1alpha1.UnderlayInterfaceTypeGroutPort,
                GroutPort: &v1alpha1.GroutPortConfig{
                    PCIAddress: &pciAddress,
                    IPAM: v1alpha1.GroutPortIPAM{
                        Addresses: []string{"192.168.100.10/24"},
                    },
                },
            }},
            Neighbors: []v1alpha1.Neighbor{{
                Address: ptr.To("192.168.100.1"),
                ASN:     65000,
            }},
        },
    }
}
```

### 9. CI workflow — `.github/workflows/ci.yaml`

Add a new job (or a new matrix entry) for the QEMU lane. This needs a runner
with KVM support. Options:

**Option A: Self-hosted runner with KVM** (recommended for real SR-IOV testing)
```yaml
qemu-e2etests:
  runs-on: [self-hosted, kvm]  # needs nested virt or bare-metal
  needs: [build-grout-images]
  steps:
    - uses: actions/checkout@v7
    - name: Download grout image
      uses: actions/download-artifact@v4
      with:
        name: image-tar-openperouter-grout
    - name: Load grout image
      run: docker load -i openperouter-grout.tar
    - name: Setup QEMU VM + TOR
      run: make qemu-deploy IMG_TAG=main-grout
    - name: Run QEMU e2e tests
      run: make qemu-e2etests GINKGO_ARGS="--label-filter='qemu-support'" TEST_ARGS="--groutmode --qemu-mode"
    - name: Cleanup
      if: always()
      run: make qemu-clean
    - name: Export logs
      if: failure()
      # SSH into VM, grab journalctl, grout logs, k3s logs, FRR logs
```

**Option B: GitHub-hosted with nested virtualization**
- `ubuntu-latest` runners have KVM available (`/dev/kvm` exists). Verified by
  many projects (e.g., Android emulator CI).
- The igb device emulation in QEMU may or may not support SR-IOV in nested
  environments — needs validation. If it works, this is the zero-cost option.

**Recommendation**: Start with Option B (GitHub-hosted). If igb VF creation
fails under nested virt, fall back to Option A. The Makefile targets and scripts
work for both — only the `runs-on` changes.

Guard the job with a label or path filter initially so it doesn't run on every
PR:
```yaml
if: contains(github.event.pull_request.labels.*.name, 'run-qemu-tests')
```

### 10. Log collection

On failure, collect:
- VM console log (QEMU serial output)
- `journalctl -u k3s` from inside the VM
- `kubectl logs` for openperouter pods
- `grcli interface show`, `grcli address show` output
- FRR TOR container logs
- `dmesg` from inside the VM (IOMMU/VFIO errors)

Script: `hack/qemu-vm/collect-logs.sh` — SSHes into VM, grabs everything,
writes to `$KIND_EXPORT_LOGS` (reusing the existing log artifact path).

## File summary

| Path | Purpose |
|------|---------|
| `hack/qemu-image/cloud-init/user-data` | Cloud-init: packages, kernel cmdline, hugepages, VFIO, k3s |
| `hack/qemu-image/cloud-init/meta-data` | Cloud-init instance metadata |
| `hack/qemu-image/prepare-image.sh` | Download base image, create cloud-init ISO |
| `hack/qemu-vm/launch-vm.sh` | Create bridge+TAP, launch QEMU |
| `hack/qemu-vm/stop-vm.sh` | Kill QEMU, remove bridge/TAP, stop FRR |
| `hack/qemu-vm/setup-vm.sh` | In-VM: create VFs, bind VFIO, start k3s, deploy openperouter |
| `hack/qemu-vm/start-tor.sh` | Start FRR TOR container on br-underlay |
| `hack/qemu-vm/collect-logs.sh` | Gather VM/k3s/grout/FRR logs on failure |
| `hack/qemu-vm/frr/frr.conf` | FRR config for TOR mock |
| `hack/qemu-vm/frr/daemons` | FRR daemons file |
| `e2etests/tests/qemu_groutport.go` | QEMU GroutPort e2e tests |
| `e2etests/tests/common.go` | Add `QEMUMode` flag |
| `e2etests/suite/suite_test.go` | Register `--qemu-mode` flag |
| `e2etests/pkg/infra/underlay_qemu.go` | QEMU-specific underlay fixtures |
| `Makefile` | Add `qemu-*` targets |
| `.github/workflows/ci.yaml` | Add `qemu-e2etests` job |

## Verification

1. **Local**: `make qemu-setup` on a machine with KVM — VM boots, k3s comes up,
   FRR TOR peers are reachable from inside the VM
2. **VF creation**: SSH into VM, verify `ls /sys/class/net/<pf>/device/virtfn*`
   shows VFs
3. **VFIO binding**: Verify VF bound to vfio-pci (`lspci -k`)
4. **E2E**: `make qemu-e2etests` — GroutPort tests pass (BGP session up,
   addresses assigned, teardown clean)
5. **CI**: Push with `run-qemu-tests` label, verify the qemu-e2etests job passes

## Open questions / follow-up

- **Nested virt igb SR-IOV**: Does QEMU's igb model support `sriov_numvfs` when
  itself running under KVM nested virt (GitHub-hosted runner)? Needs empirical
  validation. Fallback: self-hosted runner.
- **ISIS support**: The initial TOR mock only runs BGP. ISIS can be added later
  by extending the FRR config and adding test cases — the infrastructure
  supports it, only config changes needed.
- **L2VNI/L3VNI tests over DPDK**: Once GroutPort underlay works, overlay tests
  (EVPN L2/L3) can be ported to the QEMU lane. Requires adding VRFs, VXLAN
  config to the TOR, and workload containers reachable via the overlay — but
  the VM topology supports it.
- **Image caching**: The Fedora Cloud image download and cloud-init package
  install take ~2-3 minutes. A pre-baked qcow2 pushed to a registry (or cached
  in CI artifacts) would cut setup time.
