# QEMU Containerlab Architecture

## NIC Naming Convention

Each QEMU NIC follows a consistent naming scheme across host and guest:

```
Host                                         Guest (QEMU VM)
─────────────────────────────────────────────────────────────
 <name> (bridge)  ◄──►  <name>_t (tap)  |  <name> (igb)
```

For example, `toswitch1`:

```
 toswitch1 (bridge)  ◄──►  toswitch1_t (tap)  |  toswitch1 (igb)
```

The guest NIC is renamed from its kernel name (e.g. `ens4`) to the bridge
name using MAC-based udev rules written by cloud-init
(`/etc/udev/rules.d/70-persistent-net.rules`).

## NICs

| # | Bridge (host) | TAP (host)      | Guest NIC    | MAC               |
|---|---------------|-----------------|--------------|-------------------|
| 1 | toswitch1     | toswitch1_t     | toswitch1    | 52:54:00:ab:cd:01 |
| 2 | toswitch2     | toswitch2_t     | toswitch2    | 52:54:00:ab:cd:02 |
| 3 | toleafkind1   | toleafkind1_t   | toleafkind1  | 52:54:00:ab:cd:03 |
| 4 | toleafkind2   | toleafkind2_t   | toleafkind2  | 52:54:00:ab:cd:04 |

## Topology

```
┌───────────────────────────────────────────────────────────┐
│                    QEMU VM (4 igb NICs)                   │
│                                                           │
│  toswitch1    toswitch2    toleafkind1    toleafkind2     │
│     │            │             │              │           │
└─────┼────────────┼─────────────┼──────────────┼───────────┘
      │            │             │              │
  toswitch1_t  toswitch2_t  toleafkind1_t  toleafkind2_t  (TAPs)
      │            │             │              │
  toswitch1    toswitch2    toleafkind1    toleafkind2     (bridges)
      │            │             │              │
      │  uplink    │             │  uplink      │
      ▼            ▼             ▼              ▼
  leafkind1-sw     …         leafkind1         …
  (bridge)                   (FRR router)
```

## Containerlab Links

```
toleafkind1:pf0-up    ──► leafkind1:toqemu        (direct uplink)
toswitch1:pf1-up      ──► leafkind1-sw:toqemu      (via switch bridge)
leafkind1:toswitch1   ──► leafkind1-sw:leaf1
leafkind1:eth1        ──► spine:eth3
leafkind2:eth1        ──► spine:eth4
```

## Components

### QEMU VM (external)
- 4 igb NICs, one per bridge
- Connects via TAP devices
- Management NIC (virtio) for SSH and k8s API on the host loopback

### Bridges: toswitch1, toswitch2
- Each carries a single TAP to the VM
- `toswitch1` uplinks to `leafkind1-sw` bridge, which connects to `leafkind1`

### Bridges: toleafkind1, toleafkind2
- Each carries a single TAP to the VM
- `toleafkind1` uplinks directly to `leafkind1` (FRR router)

### leafkind1-sw, leafkind2-sw (bridges)
- Intermediate switch bridges between toswitch* and leaf routers

### leafkind1 (FRR router, AS 64512)
- `toqemu`: uplink from toleafkind1 bridge
- `toswitch1`: to leafkind1-sw bridge
- `eth1`: to spine:eth3

### leafkind2 (FRR router, AS 64513)
- `eth1`: to spine:eth4

### spine (FRR router, AS 64612)
- `eth3`: to leafkind1
- `eth4`: to leafkind2

## Scripts

| Script          | Purpose                                                    |
|-----------------|------------------------------------------------------------|
| `vm/launch.sh`  | Creates bridges + TAPs, launches QEMU, waits for SSH       |
| `vm/stop.sh`    | Kills QEMU, tears down TAPs and bridges, destroys clab     |
| `vm/setup.sh`   | Bootstraps k3s, deploys FRR-k8s / Multus inside the VM    |
| `deploy-clab.sh`| Deploys the containerlab topology and assigns fabric IPs   |
