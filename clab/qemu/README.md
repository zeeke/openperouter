# QEMU Integration with Containerlab

See [ARCHITECTURE.md](ARCHITECTURE.md) for the topology diagram and link map.

## Deployment

```bash
# One-command (image + clab + VM + k3s + deploy controller)
make qemu-deploy

# Or manual steps:
make qemu-image
make qemu-clab
make qemu-launch
make qemu-setup
make qemu-load-image
make qemu-e2etests
```

## TAP Device Mapping

| TAP | Bridge | NIC | VM Interface | Role |
|-----|--------|-----|--------------|------|
| qemu-tap0 | toleafkind1 | 0 | toleafkind1v0 | Trunk |
| qemu-tap1 | toleafkind1 | 1 | toleafkind1v1 | Trunk |
| qemu-tap2 | toleafkind1 | 2 | toleafkind1v2 | VLAN 33 access |
| qemu-tap3 | toleafkind1 | 3 | toleafkind1v3 | VLAN 44 access |
| qemu-tap4 | toswitch1 | 4 | toswitch1v0 | Trunk |
| qemu-tap5 | toswitch1 | 5 | toswitch1v1 | Trunk |
| qemu-tap6 | toswitch1 | 6 | toswitch1v2 | VLAN 33 access |
| qemu-tap7 | toswitch1 | 7 | toswitch1v3 | VLAN 44 access |

## Cleanup

```bash
make qemu-clean      # tear down VM + clab, preserve disk image
make qemu-destroy    # fully destroy VM, disk image, SSH keys, and clab
```

## Troubleshooting

```bash
sudo containerlab inspect --name kind
sudo bridge vlan show
ip link show | grep qemu-tap
sudo docker exec clab-kind-leafkind1 vtysh -c "show running-config"
ssh -p 2222 -i clab/qemu/vm/qemu-vm-key openperouter@localhost ip link show
```
