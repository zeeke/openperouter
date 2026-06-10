// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	underlayPortPrefix = "underlay"
)

func underlayPortName(index int) string {
	if index == 0 {
		return underlayPortPrefix
	}
	return fmt.Sprintf("%s-%d", underlayPortPrefix, index)
}

func HasUnderlayInterface(ctx context.Context, client *Client) (bool, error) {
	return client.portExists(ctx, underlayPortName(0))
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
// It moves the kernel interface into the router namespace, creates a grout
// TAP port with remote= so TC ingress rules redirect incoming packets from
// the physical interface to grout, and assigns the underlay IPs to the grout
// port. Grout handles all L2 (ARP) and L3 forwarding; it also creates a
// NOARP kernel interface for kernel TCP (used by FRR bgpd for BGP sessions).
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams) error {
	slog.DebugContext(ctx, "setup underlay", "params", params)
	defer slog.DebugContext(ctx, "setup underlay done")

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("setupUnderlay: Failed to find network namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	if err := validateNoStaleUnderlays(ns, params.UnderlayInterfaces); err != nil {
		return err
	}

	for i, underlayInterface := range params.UnderlayInterfaces {
		if err := hostnetwork.MoveInterfaceToNamespace(ctx, underlayInterface, ns); err != nil {
			return err
		}
		portName := underlayPortName(i)
		nicIndex := i
		if err := netnamespace.In(ns, func() error {
			return configureUnderlayInterface(ctx, client, ns, underlayInterface, portName, nicIndex)
		}); err != nil {
			return err
		}
	}

	if params.TunnelEndpoint != nil {
		if err := setupTunnelEndpoint(ctx, client, ns, *params.TunnelEndpoint); err != nil {
			return err
		}
	}

	return nil
}

func setupTunnelEndpoint(ctx context.Context, client *Client, ns netns.NsHandle, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, underlayPortName(0),
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	vtepIPs := make([]string, 0, 2)
	if ip := ep.IPv4CIDR; ip != "" {
		vtepIPs = append(vtepIPs, ip)
	}
	if ip := ep.IPv6CIDR; ip != "" {
		vtepIPs = append(vtepIPs, ip)
	}
	return hostnetwork.EnsureLoopback(ctx, ns, vtepIPs...)
}

func validateNoStaleUnderlays(ns netns.NsHandle, wanted []string) error {
	existingIfaces, err := hostnetwork.FindInterfacesInGroup(ns, hostnetwork.UnderlayGroupID)
	if err != nil {
		return fmt.Errorf("failed to check existing underlay interfaces: %w", err)
	}
	for _, name := range existingIfaces {
		if !slices.Contains(wanted, name) {
			return hostnetwork.UnderlayExistsError(fmt.Sprintf(
				"existing underlay found: %s, new inteUnderlayGroupIDrfaces are %v", name, wanted))
		}
	}
	return nil
}

func configureUnderlayInterface(ctx context.Context, client *Client, ns netns.NsHandle, underlayInterface, portName string, nicIndex int) error {
	underlay, err := netlink.LinkByName(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to get underlay nic by name %s: %w", underlayInterface, err)
	}
	if underlay.Attrs().Group != hostnetwork.UnderlayGroupID {
		if err := netlink.LinkSetGroup(underlay, int(hostnetwork.UnderlayGroupID)); err != nil {
			return fmt.Errorf("failed to set group ID on underlay interface %s: %w", underlayInterface, err)
		}
	}
	if err := netlink.LinkSetUp(underlay); err != nil {
		return fmt.Errorf("could not set link up for VRF %s: %v", underlay.Attrs().Name, err)
	}

	underlayAddrs, err := readUnderlayAddresses(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	tapName := "tap_" + underlayInterface
	dpdkDev := fmt.Sprintf("net_tap_ul%d", nicIndex)
	devargs := fmt.Sprintf("%s,remote=%s,iface=%s", dpdkDev, underlayInterface, tapName)
	portExists, err := client.portExists(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to check grout underlay port %s: %w", portName, err)
	}
	if !portExists {
		_ = removeLinkByName(tapName)
	}
	if err := client.ensurePort(ctx, portName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port %s: %w", portName, err)
	}

	if err := migrateAddressesToGrout(ctx, client, portName, underlayInterface, underlayAddrs); err != nil {
		return err
	}

	portAddresses, err := client.getAddresses(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to get grout underlay port addresses: %w", err)
	}
	for _, addr := range portAddresses {
		if err := ensureKernelSubnetRoute(ns, portName, addr); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}
	}

	return nil
}

func migrateAddressesToGrout(ctx context.Context, client *Client, portName, underlayInterface string, addrs []netlink.Addr) error {
	link, err := netlink.LinkByName(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to get underlay link %s: %w", underlayInterface, err)
	}

	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		if err := client.ensureAddress(ctx, portName, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", cidr, portName, err)
		}

		// Keep the address on the kernel interface so it survives pod
		// restarts (grout state is ephemeral). Set IFA_F_NOPREFIXROUTE
		// to suppress the kernel connected route — traffic must go
		// through grout's NOARP interface, not the kernel interface.
		noPfx := addr
		noPfx.Flags |= unix.IFA_F_NOPREFIXROUTE
		noPfx.LinkIndex = link.Attrs().Index
		if err := netlink.AddrReplace(link, &noPfx); err != nil {
			return fmt.Errorf("failed to set noprefixroute on %s for %s: %w", underlayInterface, cidr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "port", portName)
	}
	return nil
}

func readUnderlayAddresses(ifaceName string) ([]netlink.Addr, error) {
	var addrs []netlink.Addr

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to find underlay interface %s: %w", ifaceName, err)
	}
	all, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses on %s: %w", ifaceName, err)
	}
	for _, addr := range all {
		if addr.IP.IsLinkLocalMulticast() {
			continue
		}
		if addr.IP.IsLinkLocalUnicast() {
			continue
		}
		addrs = append(addrs, addr)
	}

	return addrs, nil
}
