// SPDX-License-Identifier:Apache-2.0

//go:build runasroot

package hostnetwork

import (
	"context"
	"net"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestMoveInterfaceToNamespaceRetries(t *testing.T) {
	tests := []struct {
		name         string
		alreadyMoved bool
		initialGroup uint32
		initialUp    bool
		desiredGroup uint32
	}{
		{name: "new move", desiredGroup: UnderlayGroupID},
		{name: "interrupted after move", alreadyMoved: true, desiredGroup: UnderlayGroupID},
		{
			name: "interrupted after group assignment", alreadyMoved: true,
			initialGroup: UnderlayGroupID, desiredGroup: UnderlayGroupID,
		},
		{
			name: "already configured", alreadyMoved: true,
			initialGroup: UnderlayGroupID, initialUp: true, desiredGroup: UnderlayGroupID,
		},
		{
			name: "existing wrong group", alreadyMoved: true,
			initialGroup: 1, initialUp: true, desiredGroup: UnderlayGroupID,
		},
		{
			name: "restore host group", alreadyMoved: true,
			initialGroup: UnderlayGroupID, initialUp: true, desiredGroup: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceNS := newLinkTestNamespace(t)
			targetNS := newLinkTestNamespace(t)
			source, err := netlink.NewHandleAt(sourceNS)
			require.NoError(t, err)
			t.Cleanup(source.Close)
			target, err := netlink.NewHandleAt(targetNS)
			require.NoError(t, err)
			t.Cleanup(target.Close)

			const name = "retry0"
			link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
			require.NoError(t, source.LinkAdd(link))
			handle := source
			if tt.alreadyMoved {
				require.NoError(t, source.LinkSetNsFd(link, int(targetNS)))
				handle = target
			}
			initial, err := handle.LinkByName(name)
			require.NoError(t, err)
			require.NoError(t, handle.LinkSetGroup(initial, int(tt.initialGroup)))
			if tt.initialUp {
				require.NoError(t, handle.LinkSetUp(initial))
			}
			addr, err := netlink.ParseAddr("192.0.2.1/24")
			require.NoError(t, err)
			require.NoError(t, handle.AddrAdd(initial, addr))

			for range 2 {
				require.NoError(t, MoveInterfaceToNamespace(context.Background(), name, source, target,
					targetNS, tt.desiredGroup))
				got, err := target.LinkByName(name)
				require.NoError(t, err)
				require.Equal(t, tt.desiredGroup, got.Attrs().Group)
				require.Equal(t, net.FlagUp, got.Attrs().Flags&net.FlagUp)
				addrs, err := target.AddrList(got, netlink.FAMILY_V4)
				require.NoError(t, err)
				require.Len(t, addrs, 1)
				require.Equal(t, addr.IPNet.String(), addrs[0].IPNet.String())
				_, err = source.LinkByName(name)
				require.ErrorAs(t, err, &netlink.LinkNotFoundError{})
			}
		})
	}
}

func newLinkTestNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := netns.Get()
	require.NoError(t, err)
	defer func() {
		require.NoError(t, netns.Set(original))
		require.NoError(t, original.Close())
	}()
	ns, err := netns.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ns.Close()) })
	return ns
}
