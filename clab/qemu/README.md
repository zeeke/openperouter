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

## NIC Mapping

| Bridge (host) | TAP (host)      | Guest NIC    | MAC               |
|---------------|-----------------|--------------|-------------------|
| toswitch1     | toswitch1_t     | toswitch1    | 52:54:00:ab:cd:01 |
| toswitch2     | toswitch2_t     | toswitch2    | 52:54:00:ab:cd:02 |
| toleafkind1   | toleafkind1_t   | toleafkind1  | 52:54:00:ab:cd:03 |
| toleafkind2   | toleafkind2_t   | toleafkind2  | 52:54:00:ab:cd:04 |

## Cleanup

```bash
make qemu-clean      # tear down VM + clab, preserve disk image
make qemu-destroy    # fully destroy VM, disk image, SSH keys, and clab
```

## Troubleshooting

```bash
sudo containerlab inspect --name kind
sudo bridge vlan show
ip link show | grep '_t'
sudo docker exec clab-kind-leafkind1 vtysh -c "show running-config"
ssh -p 2222 -i clab/qemu/vm/qemu-vm-key openperouter@localhost ip link show
```
