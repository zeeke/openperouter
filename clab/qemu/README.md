# QEMU Containerlab Environment

This environment runs one Fedora VM inside the `pe-kind-control-plane`
containerlab node. The VM hosts the k3s cluster used by the QEMU end-to-end
tests. See [ARCHITECTURE.md](ARCHITECTURE.md) for the network layout.

## Deployment

```bash
# Build the VM inputs, deploy containerlab, bootstrap k3s, load IMG, and deploy
# the controller.
make qemu-deploy

# Run individual stages when debugging.
make qemu-image
make qemu-clab
make qemu-load-image IMG=quay.io/example/image:tag
make qemu-e2etests
```

`make qemu-clab` starts QEMU and bootstraps k3s as part of the containerlab
node setup. It writes the guest kubeconfig to `bin/kubeconfig`.

## NIC Mapping

| Clab interface | TAP in QEMU container | Guest NIC    | MAC               |
|----------------|-----------------------|--------------|-------------------|
| toswitch1     | toswitch1_t     | toswitch1    | 52:54:00:ab:cd:01 |
| toswitch2     | toswitch2_t     | toswitch2    | 52:54:00:ab:cd:02 |
| toleafkind1   | toleafkind1_t   | toleafkind1  | 52:54:00:ab:cd:03 |
| toleafkind2   | toleafkind2_t   | toleafkind2  | 52:54:00:ab:cd:04 |

## Cleanup

```bash
make qemu-clean      # tear down VM + clab, preserve disk image
make qemu-destroy    # fully destroy VM, disk image, SSH keys, and clab
```

The generated image, cloud-init ISO, SSH keys, overlay disk, logs, and
`clab-kind/` state are ignored by Git.

## Troubleshooting

```bash
sudo containerlab inspect --name kind
sudo docker exec clab-kind-pe-kind-control-plane ip link show
sudo docker exec clab-kind-leafkind1 vtysh -c "show running-config"
make qemu-ssh
```
