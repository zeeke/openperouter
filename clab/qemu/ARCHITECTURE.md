# QEMU Containerlab Architecture

## Overview

QEMU runs **inside** the `pe-kind-control-plane` clab container. The
container's entrypoint bridges each clab-created interface to a TAP
device, then launches QEMU with igb NICs backed by those TAPs. The VM
is accessible via SSH and the Kubernetes API through clab port mappings.

## NIC Naming Convention

Each QEMU NIC is connected to a clab interface via a Linux bridge
inside the container:

```
Clab interface          Bridge          TAP              Guest (igb)
──────────────────────────────────────────────────────────────────────
 toswitch1       ◄──►  toswitch1_br  ◄──►  toswitch1_t  │  toswitch1
```

The guest NIC is renamed from its kernel name (e.g. `ens4`) to match
the clab interface name using MAC-based udev rules baked into
cloud-init (`/etc/udev/rules.d/70-persistent-net.rules`).

## NICs

| # | Clab interface | Bridge (container) | TAP (container) | Guest NIC   | MAC               |
|---|----------------|--------------------|-----------------|-------------|-------------------|
| 1 | toswitch1      | toswitch1_br       | toswitch1_t     | toswitch1   | 52:54:00:ab:cd:01 |
| 2 | toswitch2      | toswitch2_br       | toswitch2_t     | toswitch2   | 52:54:00:ab:cd:02 |
| 3 | toleafkind1    | toleafkind1_br     | toleafkind1_t   | toleafkind1 | 52:54:00:ab:cd:03 |
| 4 | toleafkind2    | toleafkind2_br     | toleafkind2_t   | toleafkind2 | 52:54:00:ab:cd:04 |

## Topology

```
┌──────────────── pe-kind-control-plane (clab container) ───────────────┐
│                                                                       │
│   toswitch1 ──bridge── toswitch1_t ─┐                                │
│   toswitch2 ──bridge── toswitch2_t ─┤  QEMU VM (igb NICs)           │
│   toleafkind1──bridge──toleafkind1_t┤  + virtio mgmt NIC            │
│   toleafkind2──bridge──toleafkind2_t┘  (hostfwd :2222→:22,          │
│                                          :6443→:6443)                │
└───┼────────────┼───────────┼─────────────┼────────────────────────────┘
    │            │           │             │            clab links
    ▼            ▼           ▼             ▼
 leafkind1-sw  leafkind2-sw leafkind1    leafkind2
 (bridge)      (bridge)     (FRR)        (FRR)
```

## Containerlab Links

```
leafkind1:tokindctrlpl  ──► pe-kind-control-plane:toleafkind1
leafkind2:tokindctrlpl  ──► pe-kind-control-plane:toleafkind2
pe-kind-control-plane:toswitch1 ──► leafkind1-sw:kindctrlpl1
pe-kind-control-plane:toswitch2 ──► leafkind2-sw:kindctrlpl2
leafkind1:toswitch1     ──► leafkind1-sw:leaf1
leafkind2:toswitch2     ──► leafkind2-sw:leaf2
leafkind1:eth1          ──► spine:eth3
leafkind2:eth1          ──► spine:eth4
```

## Port Mappings

| Host port | Container port | QEMU hostfwd | VM port | Purpose      |
|-----------|----------------|--------------|---------|--------------|
| 2222      | 2222           | :2222→:22    | 22      | SSH          |
| 6443      | 6443           | :6443→:6443  | 6443    | Kubernetes   |

## Scripts

| Script             | Purpose                                                   |
|--------------------|-----------------------------------------------------------|
| `vm/entrypoint.sh` | Container entrypoint: bridges + TAPs, launches QEMU       |
| `vm/setup.sh`      | Reboots for cloud-init changes and bootstraps k3s         |
| `vm/load-image.sh` | Imports a container image into k3s via SCP                |
| `vm/prepare-vm-image.sh` | Downloads and resizes the Fedora base image       |
| `vm/prepare-vm-iso.sh` | Generates the SSH key and cloud-init ISO              |
| `deploy-clab.sh`   | Deploys the containerlab topology and assigns fabric IPs  |
