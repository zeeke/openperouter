# Unprivileged VFIO Access for the Grout Container

## Problem

The grout container mounts `/dev/vfio` via hostPath, which makes the device nodes
**visible** in the container's filesystem. However, the Linux **device cgroup**
still denies `open()` on those character devices because the container runtime
never added them to the cgroup allowlist.

Error observed:
```
ERR: EAL: Cannot open VFIO container /dev/vfio/vfio, error 1 (Operation not permitted)
```

## Background: What Device Plugins / CDI Actually Do

Both mechanisms operate **at container creation time** by modifying the OCI
runtime spec before the container process starts. At the Linux level they do
two things:

1. **Make the device node visible** (filesystem) — equivalent to our hostPath
   mount of `/dev/vfio`. We already have this.

2. **Allow the device in the container's cgroup** — this is the missing piece.

### cgroup v1 (legacy)

Device access is controlled by writing rules to the container's cgroup:
```
/sys/fs/cgroup/devices/kubepods/<qos>/pod<uid>/<container-id>/devices.allow
```

Allow a character device:
```
echo "c <major>:<minor> rwm" > .../devices.allow
```

### cgroup v2 (modern — what k3s/kind/fedora use)

Device access is controlled by an eBPF program of type `BPF_PROG_TYPE_CGROUP_DEVICE`
attached to the container's cgroup. At creation time the container runtime
compiles a BPF program that whitelists specific major:minor pairs and attaches it.
There is no simple file to write; you must replace the BPF program.

## Options for Unprivileged VFIO Without a Device Plugin

### Option 1: CDI (Container Device Interface)

**When:** Device assignment is known before pod creation (static PCI topology).

CDI is supported natively by containerd ≥1.7 and CRI-O ≥1.27. It uses JSON spec
files on the node that the runtime reads at pod creation time.

1. Create a CDI spec on the node at `/etc/cdi/vfio.json`:

```json
{
  "cdiVersion": "0.6.0",
  "kind": "openperouter.io/vfio",
  "devices": [{
    "name": "default",
    "containerEdits": {
      "deviceNodes": [
        {"path": "/dev/vfio/vfio"},
        {"path": "/dev/vfio/0"}
      ]
    }
  }]
}
```

The VFIO group number can be determined at setup time:
```bash
readlink /sys/bus/pci/devices/<BDF>/iommu_group | xargs basename
```

2. Reference it from the pod via annotation:
```yaml
metadata:
  annotations:
    cdi.k8s.io/grout: "openperouter.io/vfio=default"
```

3. The runtime injects the devices AND updates the cgroup allowlist before the
   container starts.

**Pros:** Clean, Kubernetes-native, unprivileged, no BPF hacking.
**Cons:** Requires the CDI spec to exist before pod creation. Pod restart needed
if devices change.

### Option 2: Controller writes to cgroup v1 devices.allow (legacy systems only)

**When:** Running on cgroup v1 systems; dynamic device binding after pod start.

The controller (running with host cgroup access) writes allow rules directly:

```go
cgroupPath := getContainerCgroupPath(groutContainerID)
major, minor := getDeviceNumbers("/dev/vfio/vfio")
os.WriteFile(filepath.Join(cgroupPath, "devices.allow"),
    []byte(fmt.Sprintf("c %d:%d rwm", major, minor)), 0)
// Repeat for /dev/vfio/<group>
```

The controller needs:
- Access to `/sys/fs/cgroup/devices/` on the host (hostPath mount)
- Knowledge of the grout container's cgroup path (from container ID via CRI or
  `/proc/<pid>/cgroup`)

**Pros:** Works at runtime without pod restart. Straightforward on cgroup v1.
**Cons:** Only works on cgroup v1. Most modern systems use cgroup v2.

### Option 3: Controller replaces the cgroup v2 BPF device program

**When:** Running on cgroup v2 systems; dynamic device binding after pod start.

The controller must:
1. Find the grout container's cgroup path under `/sys/fs/cgroup/`
2. Query the existing `BPF_PROG_TYPE_CGROUP_DEVICE` program attached to it
3. Build a new BPF program allowing the original devices PLUS the VFIO devices
4. Attach it with `BPF_F_REPLACE` flag via `bpf()` syscall

The controller needs:
- `CAP_BPF` + `CAP_SYS_ADMIN` (or at minimum `CAP_BPF` + `CAP_NET_ADMIN`)
- Access to `/sys/fs/cgroup/` (hostPath mount)
- A BPF library (e.g., cilium/ebpf in Go)

**Pros:** Works at runtime on modern systems without pod restart.
**Cons:** Complex. Fragile (must reverse-engineer runtime's BPF program).
Requires elevated privileges on the controller.

### Option 4: Controller writes CDI spec + triggers pod restart

**When:** Hybrid approach — dynamic device discovery but willing to restart the
grout pod once.

1. Controller binds PCI device to `vfio-pci`
2. Controller determines the IOMMU group number
3. Controller writes `/etc/cdi/vfio-<node>.json` on the node (via hostPath or
   node-local file)
4. Controller deletes the router pod (DaemonSet recreates it)
5. New pod picks up CDI annotation → cgroup is correct from birth

**Pros:** Clean separation. Unprivileged grout container. No BPF hacking.
**Cons:** One-time pod restart when devices are first configured.

### Option 5: NRI (Node Resource Interface) plugin

**When:** You want a daemon on each node that hooks into container creation.

NRI plugins can intercept `CreateContainer` / `UpdateContainer` CRI calls and
modify the OCI spec, including adding devices to `linux.devices` and
`linux.resources.devices`. Supported by containerd ≥1.7 and CRI-O ≥1.26.

Write a small NRI plugin that:
1. Watches for containers with a specific annotation/label
2. Looks up which VFIO devices to inject
3. Adds them to the OCI spec at creation time

**Pros:** Dynamic. No CDI spec files to manage. No BPF from the controller.
**Cons:** Another daemon to deploy and maintain on each node.

### Option 6: Use an init container to write cgroup rules (cgroup v1 only)

**When:** cgroup v1 systems where you want a self-contained pod.

A privileged init container in the same pod modifies the cgroup before grout
starts. However, on cgroup v2 this doesn't work because init containers share
the same cgroup as regular containers and can't modify their own device BPF.

**Pros:** Self-contained, no external controller logic.
**Cons:** cgroup v1 only. Still needs `privileged: true` on the init container.

## Recommendation for the QEMU Test Environment

The QEMU VM has a fixed PCI topology (the igb VFs are always at known BDFs).
**Option 1 (CDI)** or **Option 4 (controller writes CDI + restart)** are the
best fit:

- Bind PCI devices to `vfio-pci` during VM setup (`setup.sh`)
- Write the CDI spec listing all VFIO group devices
- Add the CDI annotation to the router pod template
- The grout container starts unprivileged with full VFIO access

## Relevant Device Numbers

- `/dev/vfio/vfio`: always character device major 10, minor 196 (misc device)
- `/dev/vfio/<group>`: character device major 243 (typical, but dynamic); minor
  = group number. Check with `stat -c '%t:%T' /dev/vfio/<N>` or
  `cat /sys/class/vfio/<N>/dev`
