// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	underlayPortName = "underlay"
	underlayTapName  = "x-underlay"
	underlayBridge   = "br-underlay"
)

// underlayInterfaceSpecialAddr is used to identify the interface moved into
// the network namespace to serve the underlay.
const underlayInterfaceSpecialAddr = "172.16.1.1/32"

// HasUnderlayInterface checks whether the given namespace has an underlay
// interface configured by looking for the special marker address.
func HasUnderlayInterface(namespace string) (bool, error) {
	ns, err := netns.GetFromPath(namespace)
	if err != nil {
		return false, fmt.Errorf("HasUnderlayInterface: failed to find network namespace %s: %w", namespace, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", namespace, "error", err)
		}
	}()

	found := false
	err = netnamespace.In(ns, func() error {
		links, err := netlink.LinkList()
		if err != nil {
			return fmt.Errorf("failed to list links: %w", err)
		}
		for _, l := range links {
			addrs, _ := netlink.AddrList(l, netlink.FAMILY_ALL)
			for _, a := range addrs {
				if a.IPNet.String() == underlayInterfaceSpecialAddr {
					found = true
					return nil
				}
			}
		}
		return nil
	})
	return found, err
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
//
// Architecture: grout creates a TAP port (x-underlay) for DPDK processing.
// A Linux bridge connects the TAP to the physical underlay NIC so that
// grout's DPDK packets reach the wire. The TAP's kernel MAC is set to a
// different address than grout's port MAC to prevent the kernel from
// treating grout-originated frames as local. Kernel TCP (for BGP) flows
// through grout's TUN loopback mechanism.
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams) error {
	slog.DebugContext(ctx, "setup underlay", "params", params)
	defer slog.DebugContext(ctx, "setup underlay done")

	if params.UnderlayInterface == "" {
		return nil
	}

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupUnderlay: failed to find network namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	// Move the underlay NIC from the host namespace to the router pod's namespace.
	if err := moveInterfaceToNamespace(ctx, params.UnderlayInterface, ns); err != nil {
		return fmt.Errorf("failed to move underlay interface: %w", err)
	}

	// Mark the interface with a special address so HasUnderlayInterface can detect it.
	if err := netnamespace.In(ns, func() error {
		link, err := netlink.LinkByName(params.UnderlayInterface)
		if err != nil {
			return fmt.Errorf("failed to find underlay interface %s in namespace: %w", params.UnderlayInterface, err)
		}
		addr, err := netlink.ParseAddr(underlayInterfaceSpecialAddr)
		if err != nil {
			return err
		}
		return netlink.AddrReplace(link, addr)
	}); err != nil {
		return fmt.Errorf("failed to mark underlay interface: %w", err)
	}

	// Strip the underlay IPs from the physical interface. They will be
	// assigned to the grout port instead. The physical interface must
	// not have IPs since it will become a bridge port.
	var underlayAddrs []string
	if err := netnamespace.In(ns, func() error {
		link, err := netlink.LinkByName(params.UnderlayInterface)
		if err != nil {
			return fmt.Errorf("failed to find underlay interface: %w", err)
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("failed to list addresses: %w", err)
		}
		for _, a := range addrs {
			if a.IPNet.IP.IsLinkLocalUnicast() || a.IPNet.IP.IsLinkLocalMulticast() {
				continue
			}
			if a.IPNet.String() == underlayInterfaceSpecialAddr {
				continue
			}
			underlayAddrs = append(underlayAddrs, a.IPNet.String())
			slog.InfoContext(ctx, "stripping IP from underlay NIC", "ip", a.IPNet)
			if err := netlink.AddrDel(link, &a); err != nil {
				slog.WarnContext(ctx, "failed to remove address from underlay", "addr", a.IPNet, "error", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to strip underlay IPs: %w", err)
	}

	// Create a grout port with a named TAP (iface=). Grout reads/writes
	// from the TAP fd for DPDK processing.
	devargs := fmt.Sprintf("net_tap0,iface=%s", underlayTapName)
	if err := client.ensurePort(ctx, underlayPortName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	// Set the TAP's kernel MAC to a different address than grout's port
	// MAC. This is required to prevent the kernel from thinking that
	// grout-originated packets are local (see grout smoke tests).
	// Then create a bridge to connect the TAP to the physical NIC.
	if err := netnamespace.In(ns, func() error {
		tap, err := netlink.LinkByName(underlayTapName)
		if err != nil {
			return fmt.Errorf("TAP %s not found after creation: %w", underlayTapName, err)
		}
		mac := deterministicMAC(underlayTapName)
		slog.InfoContext(ctx, "setting TAP MAC", "tap", underlayTapName, "mac", fmt.Sprintf("%x", mac))
		if err := netlink.LinkSetHardwareAddr(tap, mac); err != nil {
			return fmt.Errorf("failed to set TAP MAC: %w", err)
		}

		return setupUnderlayBridge(ctx, params.UnderlayInterface, underlayTapName)
	}); err != nil {
		return fmt.Errorf("failed to setup underlay bridge: %w", err)
	}

	// Assign the underlay IPs to the grout port.
	for _, cidr := range underlayAddrs {
		slog.InfoContext(ctx, "assigning IP to grout underlay port", "ip", cidr)
		if err := client.addAddress(ctx, underlayPortName, cidr); err != nil {
			return fmt.Errorf("failed to assign %s to grout port: %w", cidr, err)
		}
	}

	// Grout's netlink layer adds addresses as /32 to the kernel control
	// plane interface, so no connected route exists for the underlay
	// subnet. Without a specific route the kernel uses the pod's default
	// route (via eth0), bypassing grout entirely.
	//
	// Route the underlay subnet through grout's TUN loopback ("main")
	// instead. Packets entering the TUN are processed by the DPDK graph
	// which handles ARP resolution, unlike the control plane TAP path.
	if err := netnamespace.In(ns, func() error {
		mainTUN, err := netlink.LinkByName("main")
		if err != nil {
			return fmt.Errorf("grout TUN loopback (main) not found: %w", err)
		}
		for _, cidr := range underlayAddrs {
			ip, network, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			ones, bits := network.Mask.Size()
			if ones == bits {
				continue
			}
			slog.InfoContext(ctx, "adding kernel route for underlay subnet via TUN", "dst", network, "src", ip)
			route := &netlink.Route{
				Dst:       network,
				LinkIndex: mainTUN.Attrs().Index,
				Src:       ip,
			}
			if err := netlink.RouteReplace(route); err != nil {
				slog.WarnContext(ctx, "failed to add kernel route", "dst", network, "error", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to add kernel routes for underlay: %w", err)
	}

	if params.EVPN != nil && params.EVPN.VtepIP != "" {
		// TODO: create a VRF grout port and assign the VTEP IP to it
	}

	return nil
}

// setupUnderlayBridge creates a Linux bridge and enslaves the physical
// underlay NIC and the grout TAP to it. The bridge provides L2 connectivity
// between grout's DPDK dataplane and the physical wire.
// Must be called inside the router pod's network namespace.
func setupUnderlayBridge(ctx context.Context, underlayNIC, tapName string) error {
	// Create or reuse the bridge.
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: underlayBridge}}
	link, err := netlink.LinkByName(underlayBridge)
	if err != nil {
		if !errors.As(err, &netlink.LinkNotFoundError{}) {
			return fmt.Errorf("failed to look up bridge %s: %w", underlayBridge, err)
		}
		slog.InfoContext(ctx, "creating underlay bridge", "name", underlayBridge)
		if err := netlink.LinkAdd(bridge); err != nil {
			return fmt.Errorf("failed to create bridge %s: %w", underlayBridge, err)
		}
	} else {
		var ok bool
		bridge, ok = link.(*netlink.Bridge)
		if !ok {
			// Wrong type — remove and recreate.
			if err := netlink.LinkDel(link); err != nil {
				return fmt.Errorf("failed to remove non-bridge %s: %w", underlayBridge, err)
			}
			bridge = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: underlayBridge}}
			if err := netlink.LinkAdd(bridge); err != nil {
				return fmt.Errorf("failed to create bridge %s: %w", underlayBridge, err)
			}
		}
	}

	// Re-fetch the bridge to get its index.
	bridgeLink, err := netlink.LinkByName(underlayBridge)
	if err != nil {
		return fmt.Errorf("failed to find bridge %s after creation: %w", underlayBridge, err)
	}

	// Enslave the physical NIC and the TAP to the bridge.
	for _, name := range []string{underlayNIC, tapName} {
		iface, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("failed to find %s for bridging: %w", name, err)
		}
		if iface.Attrs().MasterIndex == bridgeLink.Attrs().Index {
			slog.DebugContext(ctx, "already enslaved to bridge", "iface", name)
			continue
		}
		slog.InfoContext(ctx, "enslaving to bridge", "iface", name, "bridge", underlayBridge)
		if err := netlink.LinkSetMaster(iface, bridgeLink); err != nil {
			return fmt.Errorf("failed to enslave %s to %s: %w", name, underlayBridge, err)
		}
	}

	// Bring everything up.
	for _, name := range []string{underlayNIC, tapName, underlayBridge} {
		iface, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("failed to find %s: %w", name, err)
		}
		if err := netlink.LinkSetUp(iface); err != nil {
			return fmt.Errorf("failed to bring %s up: %w", name, err)
		}
	}

	return nil
}

// moveInterfaceToNamespace moves a network interface to the given namespace,
// preserving its IP addresses. If the interface is already in the target
// namespace, this is a no-op.
func moveInterfaceToNamespace(ctx context.Context, intf string, ns netns.NsHandle) error {
	slog.DebugContext(ctx, "move intf to namespace", "intf", intf)

	// Check if already in the target namespace.
	err := netnamespace.In(ns, func() error {
		_, err := netlink.LinkByName(intf)
		return err
	})
	if err == nil {
		slog.DebugContext(ctx, "intf is already in target namespace", "intf", intf)
		return nil
	}
	if !errors.As(err, &netlink.LinkNotFoundError{}) {
		var nsErr netnamespace.SetNamespaceError
		if errors.As(err, &nsErr) {
			return fmt.Errorf("failed to access target namespace: %w", err)
		}
	}

	// Find it in the current (host) namespace and save its addresses.
	link, err := netlink.LinkByName(intf)
	if err != nil {
		return fmt.Errorf("failed to find interface %s in host namespace: %w", intf, err)
	}

	addresses, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses for %s: %w", intf, err)
	}
	slog.DebugContext(ctx, "addresses before moving", "addresses", addresses)

	if err := netlink.LinkSetNsFd(link, int(ns)); err != nil {
		return fmt.Errorf("failed to move %s to target namespace: %w", intf, err)
	}

	// Bring it up in the target namespace and restore addresses.
	if err := netnamespace.In(ns, func() error {
		nsLink, err := netlink.LinkByName(intf)
		if err != nil {
			return fmt.Errorf("failed to find %s after move: %w", intf, err)
		}
		if err := netlink.LinkSetUp(nsLink); err != nil {
			return fmt.Errorf("failed to bring %s up: %w", intf, err)
		}

		for _, a := range addresses {
			// Clear the no-prefix-route flag to ensure connected routes are created.
			IFA_F_NOPREFIXROUTE := 0x200
			a.Flags &= ^IFA_F_NOPREFIXROUTE
			if err := netlink.AddrAdd(nsLink, &a); err != nil && !os.IsExist(err) {
				slog.WarnContext(ctx, "failed to restore address", "address", a, "error", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	slog.DebugContext(ctx, "interface moved to target namespace", "intf", intf)
	return nil
}
