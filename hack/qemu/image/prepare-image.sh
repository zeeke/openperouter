#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Downloads a Fedora Cloud base image and creates a cloud-init ISO.
# The resulting qcow2 + ISO are used by hack/qemu/vm/launch-vm.sh.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QEMU_VM_DIR="${SCRIPT_DIR}/../vm"

FEDORA_VERSION="${FEDORA_VERSION:-44}"
FEDORA_RELEASE="${FEDORA_RELEASE:-1.7}"
FEDORA_ARCH="${FEDORA_ARCH:-x86_64}"
FEDORA_IMAGE_NAME="Fedora-Cloud-Base-Generic-${FEDORA_VERSION}-${FEDORA_RELEASE}.${FEDORA_ARCH}.qcow2"
FEDORA_IMAGE_URL="https://download.fedoraproject.org/pub/fedora/linux/releases/${FEDORA_VERSION}/Cloud/${FEDORA_ARCH}/images/${FEDORA_IMAGE_NAME}"
FEDORA_ARCHIVE_URL="https://archives.fedoraproject.org/pub/archive/fedora/linux/releases/${FEDORA_VERSION}/Cloud/${FEDORA_ARCH}/images/${FEDORA_IMAGE_NAME}"

IMAGE_DIR="${QEMU_VM_DIR}"
VM_IMAGE="${IMAGE_DIR}/fedora-cloud.qcow2"
CLOUD_INIT_ISO="${IMAGE_DIR}/cloud-init.iso"
CLOUD_INIT_DIR="${SCRIPT_DIR}/cloud-init"

mkdir -p "${IMAGE_DIR}"

# Download the base image if not already present.
if [[ ! -f "${VM_IMAGE}" ]]; then
    echo "Downloading Fedora Cloud ${FEDORA_VERSION} base image..."
    if ! curl -fSL -o "${VM_IMAGE}.tmp" "${FEDORA_IMAGE_URL}"; then
        echo "Primary URL failed, trying archive mirror..."
        curl -fSL -o "${VM_IMAGE}.tmp" "${FEDORA_ARCHIVE_URL}"
    fi
    mv "${VM_IMAGE}.tmp" "${VM_IMAGE}"
    echo "Base image saved to ${VM_IMAGE}"
else
    echo "Base image already exists at ${VM_IMAGE}, skipping download."
fi

# Resize the image to give k3s and container images room.
echo "Resizing VM image to 20G..."
qemu-img resize "${VM_IMAGE}" 20G

# Create the cloud-init ISO.
echo "Creating cloud-init ISO..."
if command -v genisoimage &>/dev/null; then
    genisoimage -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_DIR}/user-data" "${CLOUD_INIT_DIR}/meta-data"
elif command -v mkisofs &>/dev/null; then
    mkisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_DIR}/user-data" "${CLOUD_INIT_DIR}/meta-data"
elif command -v xorrisofs &>/dev/null; then
    xorrisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
        "${CLOUD_INIT_DIR}/user-data" "${CLOUD_INIT_DIR}/meta-data"
else
    echo "ERROR: No ISO creation tool found (genisoimage, mkisofs, or xorrisofs)." >&2
    exit 1
fi

echo "cloud-init ISO created at ${CLOUD_INIT_ISO}"
echo "Image preparation complete."
