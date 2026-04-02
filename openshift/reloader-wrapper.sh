#!/bin/sh

# TODO: document this script and its relationship with frr-reload.py
if [ -z ${FRR_EDGE+x} ]; then
    ln -s /usr/libexec/frr/frr-reload.py /usr/lib/frr/frr-reload.py
else
    ln -sf /frr-edge/usr/sbin/frr-reload.py /usr/lib/frr/frr-reload.py
    # frr-reload.py defaults --bindir to /usr/bin, and the reloader binary
    # hardcodes /usr/bin/vtysh for liveness checks. Override with the edge
    # version so both code paths use the right vtysh.
    ln -sf /frr-edge/usr/bin/vtysh /usr/bin/vtysh
fi

/reloader-original "$@"
