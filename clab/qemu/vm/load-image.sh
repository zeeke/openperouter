#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Imports a container image into k3s running inside the QEMU VM.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "${SCRIPT_DIR}/../qemu-common.sh"

IMG="${1:?Usage: load-image.sh <image-ref> [tar-path]}"
TAR_PATH="${2:-}"

if [[ -z "${TAR_PATH}" ]]; then
    TAR_PATH=$(mktemp /tmp/qemu-image-XXXXXX.tar)
    echo "Saving ${IMG} to ${TAR_PATH}..."
    docker save -o "${TAR_PATH}" "${IMG}"
    CLEANUP_TAR=true
else
    CLEANUP_TAR=false
fi

echo "Copying image to VM..."
${SCP_CMD} "${TAR_PATH}" openperouter@localhost:/tmp/openperouter.tar

echo "Importing image into k3s..."
${SSH_CMD} "sudo k3s ctr images import /tmp/openperouter.tar && sudo rm -f /tmp/openperouter.tar"

if [[ "${CLEANUP_TAR}" == "true" ]]; then
    rm -f "${TAR_PATH}"
fi

echo "Image ${IMG} loaded into QEMU VM."
