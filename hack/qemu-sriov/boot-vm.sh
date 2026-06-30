#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

if [ ! -f "$QEMU_WORKDIR/base.img" ]; then
  echo "Downloading Ubuntu 24.04 cloud image…"
  curl -fSL -o "$QEMU_WORKDIR/base.img" https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
fi

qemu-img create -f qcow2 -F qcow2 -b "$QEMU_WORKDIR/base.img" "$QEMU_WORKDIR/disk.qcow2"
qemu-img resize "$QEMU_WORKDIR/disk.qcow2" +6G

# 6144Mi leaves ~3Gi for the kernel/k3s/OS after cloud-init's
# configure-sriov.sh reserves 3Gi (1536 * 2Mi) of hugepages.
qemu-system-x86_64 \
  -machine q35,accel=kvm \
  -cpu host \
  -m 6144 -smp 2 \
  -drive file="$QEMU_WORKDIR/disk.qcow2",if=virtio,format=qcow2 \
  -drive file="$QEMU_WORKDIR/seed.img",if=virtio,format=raw \
  -netdev user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::6443-:6443 \
  -device virtio-net-pci,netdev=net0 \
  -netdev user,id=net1,net=10.0.3.0/24 \
  -device pcie-root-port,id=pcie_port.1 \
  -device igb,bus=pcie_port.1,netdev=net1 \
  -display none \
  -serial file:"$QEMU_WORKDIR/console.log" \
  -daemonize \
  -pidfile "$QEMU_WORKDIR/qemu.pid"
