#!/bin/sh

# TODO: document this script and its relationship with frr-reload.py
if [ -z ${FRR_EDGE+x} ]; then
    ln -s /usr/libexec/frr/frr-reload.py /usr/lib/frr/frr-reload.py
else
    ln -sf /frr-edge/usr/sbin/frr-reload.py /usr/lib/frr/frr-reload.py
fi

# This is needed by frr-reload.py to find the right `vtysh` instance.
export PATH="/frr-edge/usr/bin:${PATH}"

/reloader-original "$@"
