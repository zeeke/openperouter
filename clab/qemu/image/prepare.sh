#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Downloads a Fedora Cloud base image and/or creates a cloud-init ISO.
# Usage: prepare.sh [image|iso]
# With no argument, prepares both. The resulting qcow2 + ISO are used by
# clab/qemu/vm/launch.sh.

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

prepare_image() {
    mkdir -p "${IMAGE_DIR}"
    # qemu-img resize needs an exclusive write lock, so skip when the image
    # already exists (a running VM may hold it).
    if [[ -f "${VM_IMAGE}" ]]; then
        echo "Base image already exists at ${VM_IMAGE}, skipping download and resize."
        return
    fi

    echo "Downloading Fedora Cloud ${FEDORA_VERSION} base image... (${FEDORA_IMAGE_URL})"
    if ! curl -fSL -o "${VM_IMAGE}.tmp" "${FEDORA_IMAGE_URL}"; then
        echo "Primary URL failed, trying archive mirror... (${FEDORA_ARCHIVE_URL})"
        curl -fSL -o "${VM_IMAGE}.tmp" "${FEDORA_ARCHIVE_URL}"
    fi
    mv "${VM_IMAGE}.tmp" "${VM_IMAGE}"
    echo "Base image saved to ${VM_IMAGE}"
    echo "Resizing VM image to 20G..."
    qemu-img resize "${VM_IMAGE}" 20G
}

prepare_iso() {
    mkdir -p "${IMAGE_DIR}"

    local ssh_key="${QEMU_VM_DIR}/qemu-vm-key"
    if [[ ! -f "${ssh_key}" ]]; then
        echo "Generating SSH key at ${ssh_key}..."
        ssh-keygen -t ed25519 -f "${ssh_key}" -N "" -q
    fi

    if [[ -f "${CLOUD_INIT_ISO}" ]]; then
        echo "cloud-init ISO already exists at ${CLOUD_INIT_ISO}, skipping."
        return
    fi

    echo "Preparing cloud-init data..."
    local build_dir
    build_dir=$(mktemp -d)
    
    # Copy static meta-data
    cp "${CLOUD_INIT_DIR}/meta-data" "${build_dir}/meta-data"

    # Inject the generated public key into user-data
    local pub_key
    pub_key="$(cat "${ssh_key}.pub")"
    sed "s|ssh_authorized_keys: \[\]|ssh_authorized_keys:\n      - ${pub_key}|g" "${CLOUD_INIT_DIR}/user-data" > "${build_dir}/user-data"

    echo "Creating cloud-init ISO..."
    if command -v genisoimage &>/dev/null; then
        genisoimage -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
            "${build_dir}/user-data" "${build_dir}/meta-data"
    elif command -v mkisofs &>/dev/null; then
        mkisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
            "${build_dir}/user-data" "${build_dir}/meta-data"
    elif command -v xorrisofs &>/dev/null; then
        xorrisofs -output "${CLOUD_INIT_ISO}" -volid cidata -joliet -rock \
            "${build_dir}/user-data" "${build_dir}/meta-data"
    else
        echo "ERROR: No ISO creation tool found (genisoimage, mkisofs, or xorrisofs)." >&2
        rm -rf "${build_dir}"
        exit 1
    fi

    rm -rf "${build_dir}"
    echo "cloud-init ISO created at ${CLOUD_INIT_ISO}"
}

case "${1:-all}" in
    image)
        prepare_image
        ;;
    iso)
        prepare_iso
        ;;
    all)
        prepare_image
        prepare_iso
        echo "Image preparation complete."
        ;;
    *)
        echo "Usage: $0 [image|iso]" >&2
        exit 1
        ;;
esac
