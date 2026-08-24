# QEMU Containerlab Architecture

## Topology

```
┌─────────────────────────────────────────────────────┐
│               QEMU VM (8 igb NICs)                  │
│                                                     │
│  toleafkind1 PF          toswitch1 PF               │
│  (NICs 0-3)              (NICs 4-7)                 │
│  ┌───┬───┬───┬───┐      ┌───┬───┬───┬───┐           │
│  │ 0 │ 1 │ 2 │ 3 │      │ 4 │ 5 │ 6 │ 7 │           │
│  │trk│trk│V33│V44│      │trk│trk│V33│V44│           │
│  └─┬─┴─┬─┴─┬─┴─┬─┘      └─┬─┴─┬─┴─┬─┴─┬─┘           │
└────┼───┼───┼───┼──────────┼───┼───┼───┼─────────────┘
     │   │   │   │          │   │   │   │
   TAP0 TAP1 TAP2 TAP3    TAP4 TAP5 TAP6 TAP7
     │   │   │   │          │   │   │   │
     └───┴───┴───┘          └───┴───┴───┘
          │                     │
  ┌───────┴───────┐      ┌──────┴──────┐
  │ toleafkind1   │      │  toswitch1  │
  │ (bridge)      │      │  (bridge)   │
  │ +VLAN filter  │      │ +VLAN filter│
  └───────┬───────┘      └──────┬──────┘
          │ uplink              │ uplink
          │                     │
  ┌───────┴───────┐      ┌──────┴──────┐
  │               │      │ leafkind1-sw│
  │               │      │  (bridge)   │
  │               │      └──────┬──────┘
  │               │             │ leaf1
  │  leafkind1    │◄────────────┘
  │  (FRR router) │
  │  AS 64512     │
  └───────┬───────┘
          │ eth1
   ┌──────┴──────┐
   │    spine    │
   │ (FRR router)│
   │  AS 64612   │
   └─────────────┘
```

## Links

```
toleafkind1:uplink    ──► leafkind1:toqemu
toswitch1:uplink      ──► leafkind1-sw:toqemu
leafkind1:toswitch1   ──► leafkind1-sw:leaf1
leafkind1:eth1        ──► spine:eth3
leafkind2:eth1        ──► spine:eth4
```

## Components

### QEMU VM (external)
- 8 igb NICs (2 PFs × 4 NICs each)
- Connects via TAP devices

### Fake PF 1: toleafkind1
- Bridge with VLAN filtering
- 4 TAPs: 0, 1 (trunk), 2 (VLAN 33), 3 (VLAN 44)
- VM interfaces: toleafkind1v0, v1, v2, v3
- 1 uplink: direct to leafkind1:toqemu

### Fake PF 2: toswitch1
- Bridge with VLAN filtering
- 4 TAPs: 4, 5 (trunk), 6 (VLAN 33), 7 (VLAN 44)
- VM interfaces: toswitch1v0, v1, v2, v3
- 1 uplink: to leafkind1-sw:toqemu

### leafkind1 (FRR router, AS 64512)
- `toqemu`: direct uplink from toleafkind1 PF
- `toswitch1`: to leafkind1-sw bridge
- `eth1`: to spine:eth3

### leafkind2 (FRR router, AS 64513)
- `eth1`: to spine:eth4

### leafkind1-sw (bridge)
- `toqemu`: from toswitch1 PF
- `leaf1`: to leafkind1:toswitch1

### leafkind2-sw (bridge)

### spine (FRR router, AS 64612)
- `eth3`: to leafkind1
- `eth4`: to leafkind2

## VLAN Configuration

Both fake PF bridges use identical VLAN setup:

**Trunk ports** (TAPs 0, 1, 4, 5):
- PVID 1 (default VLAN), VLANs 33 and 44 tagged

**VLAN 33 access** (TAPs 2, 6):
- PVID 33, untagged, no VLAN 1

**VLAN 44 access** (TAPs 3, 7):
- PVID 44, untagged, no VLAN 1
