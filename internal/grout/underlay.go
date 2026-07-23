// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"syscall"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/openperouter/openperouter/internal/sriov"
	"github.com/openperouter/openperouter/internal/sysctl"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	UnderlayPortNamePrefix = "u_"
)

// SetupUnderlay configures the underlay interfaces via the grout dataplane.
// Every interface is provisioned in the router namespace according to its
// kind (host network devices are moved in, CNI interfaces are added by
// invoking their plugin); then a grout TAP port with remote= is created so
// TC ingress rules redirect incoming packets from the interface to grout,
// and the underlay IPs are moved to the grout port. Grout handles all L2
// (ARP) and L3 forwarding; it also creates a NOARP kernel interface for
// kernel TCP (used by FRR bgpd for BGP sessions).
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams) error {
	slog.DebugContext(ctx, "setup underlay", "params", params)
	defer slog.DebugContext(ctx, "setup underlay done")

	perouterNetNS, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("setupUnderlay: Failed to find network namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := perouterNetNS.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	// If any existing underlay interfaces were removed from the new list,
	// clean them up before setting up the new ones, tearing down their
	// grout state first.
	existing, err := hostnetwork.UnderlayInterfaces(params.TargetNS)
	if err != nil {
		return fmt.Errorf("failed to check existing underlay interfaces: %w", err)
	}
	if toRemove := hostnetwork.UnderlayInterfacesToRemove(existing, params.UnderlayInterfaces); len(toRemove) > 0 {
		slog.InfoContext(ctx, "underlay interfaces changed, removing old interfaces before setup",
			"toRemove", toRemove, "requested", params.UnderlayInterfaces)
		if err := RestoreUnderlay(ctx, client, params.TargetNS, toRemove); err != nil {
			return fmt.Errorf("failed to remove old underlay interfaces: %w", err)
		}
	}

	for _, iface := range params.UnderlayInterfaces {
		switch iface.Kind {
		case hostnetwork.UnderlayInterfaceNetDev:
			if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, iface); err != nil {
				return err
			}
			if err := netnamespace.In(perouterNetNS, func() error {
				return configureUnderlayPort(ctx, client, iface.InterfaceName)
			}); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := hostnetwork.SetupUnderlayCNIDevInterface(ctx, params.TargetNS, iface); err != nil {
				return err
			}
			if err := netnamespace.In(perouterNetNS, func() error {
				return configureUnderlayPort(ctx, client, iface.InterfaceName)
			}); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceGroutPort:
			if iface.GroutPort == nil {
				return fmt.Errorf("groutPort params missing for interface %s", iface.InterfaceName)
			}
			if err := prepareGroutPortDriver(ctx, perouterNetNS, iface.GroutPort.PCIAddress); err != nil {
				return fmt.Errorf("failed to prepare grout port driver for %s: %w", iface.GroutPort.PCIAddress, err)
			}
			if err := netnamespace.In(perouterNetNS, func() error {
				return configureGroutPort(ctx, client, iface)
			}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface %s has unsupported kind %q", iface.InterfaceName, iface.Kind)
		}
	}

	if params.TunnelEndpoint != nil {
		if err := setupTunnelEndpoint(ctx, client, *params.TunnelEndpoint); err != nil {
			return err
		}
	}

	return nil
}

func setupTunnelEndpoint(ctx context.Context, client *Client, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, "main",
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	return nil
}

// RestoreUnderlay removes the given underlay interfaces: it tears
// down their grout ports first (migrating the underlay addresses back to the
// kernel interfaces), then removes the interfaces from the namespace
// according to how they were provisioned, exactly like the kernel datapath
// does.
func RestoreUnderlay(
	ctx context.Context,
	client *Client,
	targetNS string,
	toRemove []hostnetwork.UnderlayInterface,
) error {
	if len(toRemove) == 0 {
		return nil
	}

	interfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to list grout interfaces: %w", err)
	}
	existingPorts := map[string]struct{}{}
	for _, iface := range interfaces {
		existingPorts[iface.Name] = struct{}{}
	}

	ns, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to find network namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()

	var kernelInterfaces []hostnetwork.UnderlayInterface
	for _, iface := range toRemove {
		portName := UnderlayPortNamePrefix + iface.InterfaceName
		if _, found := existingPorts[portName]; !found {
			slog.Debug("RestoreUnderlay: port already removed", "namespace", targetNS, "port", portName)
			continue
		}

		if iface.Kind == hostnetwork.UnderlayInterfaceGroutPort {
			if err := removeGroutPortAddresses(ctx, client, ns, portName); err != nil {
				return err
			}
			if iface.GroutPort != nil && iface.GroutPort.NetlinkDevice != "" {
				kernelInterfaces = append(kernelInterfaces, hostnetwork.UnderlayInterface{
					InterfaceName: iface.GroutPort.NetlinkDevice,
					Kind:          hostnetwork.UnderlayInterfaceNetDev,
				})
			}
		} else {
			if err := netnamespace.In(ns, func() error {
				if err := migrateAddressesToKernel(ctx, client, portName); err != nil {
					return fmt.Errorf("RestoreUnderlay: failed to migrate addresses back to kernel: %w", err)
				}

				return nil
			}); err != nil {
				return err
			}
			kernelInterfaces = append(kernelInterfaces, iface)
		}

		if err := client.deletePort(ctx, portName); err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to delete grout port %s: %w", portName, err)
		}
	}

	if len(kernelInterfaces) > 0 {
		if err := hostnetwork.RestoreUnderlay(ctx, targetNS, kernelInterfaces); err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
		}
	}

	return nil
}

func removeGroutPortAddresses(ctx context.Context, client *Client, ns netns.NsHandle, portName string) error {
	return netnamespace.In(ns, func() error {
		addrs, err := client.getAddresses(ctx, portName)
		if err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to get addresses for grout port %s: %w", portName, err)
		}
		for _, addr := range addrs {
			if err := removeKernelSubnetRoute("main", addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to remove kernel route for %s: %w", addr, err)
			}
			if err := client.deleteAddress(ctx, portName, addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to delete address %s from grout port %s: %w", addr, portName, err)
			}
		}
		return nil
	})
}

func configureUnderlayPort(ctx context.Context, client *Client, underlayInterface string) error {
	underlayAddrs, err := hostnetwork.AddressesForInterface(underlayInterface, hostnetwork.ExcludeLinkLocal())
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	devargs := fmt.Sprintf("net_tap%s,remote=%s,iface=%s", makeTapRandomString(), underlayInterface, "tap_"+underlayInterface)
	if err := client.ensurePort(ctx, UnderlayPortNamePrefix+underlayInterface, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	if err := migrateAddressesToGrout(ctx, client, underlayInterface, underlayAddrs); err != nil {
		return err
	}

	return nil
}

// configureGroutPort creates a DPDK port in grout directly from a PCI
// device address, assigns the inline IPAM addresses, and sets up the
// kernel routes needed by FRR.
func configureGroutPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface) error {
	if iface.GroutPort == nil {
		return fmt.Errorf("groutPort params missing for interface %s", iface.InterfaceName)
	}

	portName := UnderlayPortNamePrefix + iface.InterfaceName
	opts := PortOptions{
		MTU:      iface.GroutPort.MTU,
		RXQueues: iface.GroutPort.RXQueues,
		QSize:    iface.GroutPort.QSize,
	}

	if err := client.ensurePortWithOptions(ctx, portName, iface.GroutPort.PCIAddress, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
	}

	for _, addr := range iface.GroutPort.Addresses {
		if err := client.ensureAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}

		if err := ensureKernelSubnetRoute("main", addr); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "configured grout DPDK port address", "cidr", addr, "port", portName)
	}

	if err := sysctl.Ensure(sysctl.DisableRPFilter(portName)); err != nil {
		return fmt.Errorf("failed to disable rp_filter on %s: %w", portName, err)
	}

	return nil
}

// prepareGroutPortDriver inspects the driver bound to a PCI device and
// takes the appropriate action:
//   - Intel kernel drivers (igb, iavf, ice, i40e): rebind to vfio-pci
//   - vfio-pci: already bound, nothing to do
//   - mlx5_core: move the kernel netlink interface to the perouter namespace (bifurcated driver)
//   - unknown/unbound: warn and proceed (let grout handle it)
func prepareGroutPortDriver(ctx context.Context, perouterNetNS netns.NsHandle, pciAddr string) error {
	driver, err := sriov.GetPCIDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to get PCI driver for %s: %w", pciAddr, err)
	}

	switch {
	case sriov.IntelKernelDrivers[driver]:
		if err := sriov.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to rebind PCI device %s from %s to vfio-pci: %w",
				pciAddr, driver, err)
		}
		return nil

	case driver == sriov.DriverVFIOPCI:
		return nil

	case driver == sriov.DriverMlx5Core:
		netdev, err := sriov.GetPCINetDevice(pciAddr)
		if err != nil {
			return fmt.Errorf("mlx5 PCI device %s has no kernel netlink interface: %w", pciAddr, err)
		}

		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, hostnetwork.UnderlayInterface{
			InterfaceName: netdev,
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
		}); err != nil {
			return fmt.Errorf("failed to move mlx5 netlink device %s to namespace: %w", netdev, err)
		}
		return nil

	default:
		slog.Warn("unknown driver for GroutPort PCI device, proceeding with DPDK",
			"pciAddress", pciAddr, "driver", driver)
		return nil
	}
}

func migrateAddressesToGrout(ctx context.Context, client *Client, underlayInterface string, addrs []netlink.Addr) error {
	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		// Move the address to the grout underlay port, so grout can register routes and nexthops
		if err := client.ensureAddress(ctx, UnderlayPortNamePrefix+underlayInterface, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout underlay port: %w", cidr, err)
		}

		if err := hostnetwork.DeleteAddressFromInterface(underlayInterface, addr); err != nil {
			return fmt.Errorf("failed to remove address %s from underlay interface: %w", cidr, err)
		}

		// FRR needs kernel routes to enstabilish BGP connections. Grout requires that all the kernel
		// traffic must enter grout via the `main` TAP device.
		if err := ensureKernelSubnetRoute("main", addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", UnderlayPortNamePrefix+underlayInterface)
	}

	// for each port, grout creates a NOARP kernel interface to make FRR zebra daemon work.
	// 5: u_enp3s0: <BROADCAST,MULTICAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
	//    link/ether 00:09:a8:38:8e:3b brd ff:ff:ff:ff:ff:ff promiscuity 0 allmulti 0 minmtu 68 maxmtu 65521
	//    tun type tap ...
	//    alias Grout control plane interface
	// bgpd packets will leave through the `main` intrerface and will come back on the `u_xxx` interface, hence the
	// need to disable rp_filter on the `u_xxx` interface.
	if err := sysctl.Ensure(sysctl.DisableRPFilter(UnderlayPortNamePrefix + underlayInterface)); err != nil {
		return fmt.Errorf("failed to disable rp_filter on underlay interface %s: %w", UnderlayPortNamePrefix+underlayInterface, err)
	}

	return nil
}

func migrateAddressesToKernel(ctx context.Context, client *Client, underlayPortName string) error {
	addrs, err := client.getAddresses(ctx, underlayPortName)
	if err != nil {
		return fmt.Errorf("failed to read addresses from grout port %s: %w", underlayPortName, err)
	}
	if len(addrs) == 0 {
		return nil
	}

	underlayLinkName := strings.Replace(underlayPortName, UnderlayPortNamePrefix, "", 1)

	link, err := netlink.LinkByName(underlayLinkName)
	if err != nil {
		return fmt.Errorf("failed to find link %s: %w", underlayLinkName, err)
	}

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address %s: %w", addr, err)
		}
		if ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}

		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to link %s: %w", addr, underlayLinkName, err)
		}
	}

	for _, addr := range addrs {
		if err := removeKernelSubnetRoute("main", addr); err != nil {
			return fmt.Errorf("failed to remove kernel route for %s: %w", addr, err)
		}

		if err := client.deleteAddress(ctx, underlayPortName, addr); err != nil {
			return fmt.Errorf("failed to delete addresses on grout port %s: %w", underlayPortName, err)
		}

	}

	return nil
}

func ensureKernelSubnetRoute(ifaceName, addr string) error {
	route, err := connectedRouteForAddress(ifaceName, addr)
	if err != nil {
		return err
	}
	if route == nil {
		return nil
	}

	existing, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, route, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("failed to list routes for %s dev %s: %w", route.Dst, ifaceName, err)
	}
	if len(existing) > 0 {
		return nil
	}

	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("failed to add route for %s dev %s: %w", route.Dst, ifaceName, err)
	}

	slog.Info("added kernel route for subnet", "cidr", addr, "src", route.Src, "ipnet", route.Dst, "iface", ifaceName)
	return nil
}

func removeKernelSubnetRoute(ifaceName, addr string) error {
	route, err := connectedRouteForAddress(ifaceName, addr)
	if err != nil {
		return err
	}
	if route == nil {
		return nil
	}

	if err := netlink.RouteDel(route); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("failed to delete route for %s dev %s: %w", route.Dst, ifaceName, err)
	}

	slog.Info("removed kernel route for subnet", "cidr", addr, "src", route.Src, "ipnet", route.Dst, "iface", ifaceName)
	return nil
}

// assignIPsToGroutPort assigns IPv4 and IPv6 addresses to a grout port via grcli.
func assignIPsToGroutPort(ctx context.Context, client *Client, portName string, ipv4, ipv6 string) error {
	if ipv4 == "" && ipv6 == "" {
		return fmt.Errorf("at least one IP address must be provided (IPv4 or IPv6)")
	}

	for _, addr := range []string{ipv4, ipv6} {
		if addr == "" {
			continue
		}
		slog.DebugContext(ctx, "assigning IP to grout port", "port", portName, "addr", addr)
		if err := client.ensureAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}
	}
	return nil
}

func connectedRouteForAddress(ifaceName, addr string) (*netlink.Route, error) {
	srcAddr, ipNet, err := net.ParseCIDR(addr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CIDR %s: %w", addr, err)
	}
	ones, bits := ipNet.Mask.Size()
	if ones == bits {
		return nil, nil
	}
	if srcAddr.IsLinkLocalUnicast() {
		return nil, nil
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %s: %w", ifaceName, err)
	}

	return &netlink.Route{
		Dst:       ipNet,
		LinkIndex: link.Attrs().Index,
		Src:       srcAddr,
	}, nil
}
