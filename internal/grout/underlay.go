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

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/openperouter/openperouter/internal/pci"
	"github.com/openperouter/openperouter/internal/sysctl"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	UnderlayPortNamePrefix             = "u_"
	UnderlayInterfaceDescriptionMarker = "underlay"
)

// SetupUnderlay configures the underlay interfaces via the grout dataplane.
// Every interface is provisioned in the router namespace according to its
// kind (host network devices are moved in, CNI interfaces are added by
// invoking their plugin); then a grout TAP port with remote= is created so
// TC ingress rules redirect incoming packets from the interface to grout,
// and the underlay IPs are moved to the grout port. Grout handles all L2
// (ARP) and L3 forwarding; it also creates a NOARP kernel interface for
// kernel TCP (used by FRR bgpd for BGP sessions).
//
// When AcceleratedConfig is set, the kernel netdev is bound as a DPDK port
// instead of using TAP+remote=.
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
	existing, err := UnderlayInterfaces(ctx, client, params.TargetNS)
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
			if iface.AcceleratedConfig != nil {
				if err := setupGroutPortUnderlay(ctx, client, perouterNetNS, iface); err != nil {
					return err
				}
				continue
			}
			if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := hostnetwork.SetupUnderlayCNIDevInterface(ctx, params.TargetNS, iface); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface %s has unsupported kind %q", iface.InterfaceName, iface.Kind)
		}
		if err := netnamespace.In(perouterNetNS, func() error {
			return configureUnderlayPort(ctx, client, iface.InterfaceName)
		}); err != nil {
			return err
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
	if err := assignIPsToGroutPort(ctx, client, defaultVRFName,
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	return nil
}

func setupGroutPortUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	state, err := captureDeviceState(ctx, iface.InterfaceName)
	if err != nil {
		return err
	}

	if err := prepareGroutPortDriver(ctx, perouterNetNS, state.PCIAddress, state.InterfaceName); err != nil {
		return fmt.Errorf("failed to prepare grout port driver for %s: %w", state.PCIAddress, err)
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureGroutPort(ctx, client, iface, state.PCIAddress, state.Addresses)
	})
}

func captureDeviceState(ctx context.Context, netlinkName string) (*devicestate.Entry, error) {
	devState, err := devicestate.Load(devicestate.Entry{InterfaceName: netlinkName})
	if err != nil {
		return nil, fmt.Errorf("failed to load device state for %s: %w", netlinkName, err)
	}
	if devState.PCIAddress != "" {
		return devState, nil
	}

	pciAddr, err := pci.ResolveNetlinkName(netlinkName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PCI address for %s: %w", netlinkName, err)
	}
	driver, err := pci.GetPCIDriver(pciAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to read driver for %s: %w", pciAddr, err)
	}

	netlinkAddrs, err := hostnetwork.AddressesForInterface(netlinkName, hostnetwork.ExcludeLinkLocal())
	if err != nil {
		return nil, fmt.Errorf("failed to read addresses from %s: %w", netlinkName, err)
	}
	addrs := make([]string, 0, len(netlinkAddrs))
	for _, a := range netlinkAddrs {
		addrs = append(addrs, a.IPNet.String())
	}

	*devState = devicestate.Entry{
		InterfaceName:  netlinkName,
		PCIAddress:     pciAddr,
		OriginalDriver: driver,
		Addresses:      addrs,
	}
	if err := devicestate.Save(*devState); err != nil {
		return nil, fmt.Errorf("failed to save device state for %s: %w", netlinkName, err)
	}

	for _, a := range netlinkAddrs {
		if err := hostnetwork.DeleteAddressFromInterface(netlinkName, a); err != nil {
			slog.WarnContext(ctx, "failed to remove address from kernel netdev before DPDK bind",
				"addr", a.IPNet.String(), "iface", netlinkName, "error", err)
		}
	}

	return devState, nil
}

// UnderlayInterfaces returns TAP underlay netdevs in the namespace plus
// DPDK-bound grout ports reconstructed from grout and the saved device state.
func UnderlayInterfaces(ctx context.Context, client *Client, namespace string) ([]hostnetwork.UnderlayInterface, error) {
	ret, err := hostnetwork.UnderlayInterfaces(namespace)
	if err != nil {
		return nil, err
	}

	groutInterfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return nil, err
	}

	for _, groutIface := range groutInterfaces {
		details, err := client.getInterfaceDetails(ctx, groutIface.Name)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(details.Description, UnderlayInterfaceDescriptionMarker) {
			continue
		}
		if !pci.IsPCIAddress(details.Devargs) {
			continue
		}
		iface, err := groutPortToUnderlayInterface(groutIface, details)
		if err != nil {
			return nil, err
		}
		ret = append(ret, iface)
	}
	return ret, nil
}

// groutPortToUnderlayInterface reconstructs the host underlay interface
// from a PCI-backed grout port using the saved device state.
func groutPortToUnderlayInterface(groutIface groutInterface, details *groutInterfaceDetails) (hostnetwork.UnderlayInterface, error) {
	state, err := devicestate.LoadByPCI(details.Devargs)
	if err != nil {
		return hostnetwork.UnderlayInterface{}, fmt.Errorf("failed to load device state for grout port %s: %w", groutIface.Name, err)
	}
	if state.InterfaceName == "" {
		return hostnetwork.UnderlayInterface{}, fmt.Errorf("device state for grout port %s has no interface name", groutIface.Name)
	}
	return hostnetwork.UnderlayInterface{
		InterfaceName:     state.InterfaceName,
		Kind:              hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{},
	}, nil
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

	var tapToRemove []hostnetwork.UnderlayInterface
	for _, iface := range toRemove {
		if iface.AcceleratedConfig != nil {
			if err := teardownGroutPortUnderlay(ctx, client, ns, targetNS, existingPorts, iface); err != nil {
				return err
			}
			continue
		}

		portName := portName(iface)
		if _, found := existingPorts[portName]; !found {
			slog.Debug("RestoreUnderlay: port already removed", "namespace", targetNS, "port", portName)
			tapToRemove = append(tapToRemove, iface)
			continue
		}

		if err := netnamespace.In(ns, func() error {
			return migrateAddressesToKernel(ctx, client, portName)
		}); err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to migrate addresses back to kernel: %w", err)
		}

		if err := client.deletePort(ctx, portName); err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to delete grout port %s: %w", portName, err)
		}
		tapToRemove = append(tapToRemove, iface)
	}

	if len(tapToRemove) == 0 {
		return nil
	}
	if err := hostnetwork.RestoreUnderlay(ctx, targetNS, tapToRemove); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
	}

	return nil
}

func teardownGroutPortUnderlay(ctx context.Context, client *Client, ns netns.NsHandle, targetNS string, existingPorts map[string]struct{}, iface hostnetwork.UnderlayInterface) error {
	portName := portName(iface)
	if _, found := existingPorts[portName]; found {
		if err := netnamespace.In(ns, func() error {
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
		}); err != nil {
			return err
		}

		if err := client.deletePort(ctx, portName); err != nil {
			return fmt.Errorf("failed to delete grout port %s: %w", portName, err)
		}
	}

	state, err := devicestate.Load(devicestate.Entry{InterfaceName: iface.InterfaceName})
	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", iface.InterfaceName, err)
	}
	if state.PCIAddress == "" {
		slog.WarnContext(ctx, "no saved device state, cannot restore driver/IPs",
			"interfaceName", iface.InterfaceName)
		return nil
	}

	if pci.IsBifurcated(state.OriginalDriver) {
		if err := hostnetwork.RestoreUnderlayNetDevInterface(ctx, targetNS, iface.InterfaceName); err != nil {
			return fmt.Errorf("failed to move bifurcated netdev %s back to the host namespace: %w",
				iface.InterfaceName, err)
		}
	} else if state.OriginalDriver != "" && state.OriginalDriver != pci.DriverVFIOPCI {
		if err := pci.RestoreDriver(state.PCIAddress, state.OriginalDriver); err != nil {
			return fmt.Errorf("failed to restore driver %s on %s: %w",
				state.OriginalDriver, state.PCIAddress, err)
		}
		slog.InfoContext(ctx, "restored original driver",
			"pciAddress", state.PCIAddress, "driver", state.OriginalDriver)
	}

	if err := restoreIPAddresses(ctx, *state); err != nil {
		return err
	}

	if err := devicestate.Delete(devicestate.Entry{InterfaceName: iface.InterfaceName}); err != nil {
		return fmt.Errorf("failed to delete device state file for %s: %w", iface.InterfaceName, err)
	}

	return nil
}

func restoreIPAddresses(ctx context.Context, state devicestate.Entry) error {
	if state.InterfaceName == "" || len(state.Addresses) == 0 {
		return nil
	}
	link, err := netlink.LinkByName(state.InterfaceName)
	if err != nil {
		return fmt.Errorf("kernel netdev [%s] not found, cannot re-apply IPs: %w", state.InterfaceName, err)
	}
	for _, addr := range state.Addresses {
		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			return fmt.Errorf("failed to restore IP address %s to kernel netdev [%s]: %w", addr, state.InterfaceName, err)
		}
		slog.InfoContext(ctx, "restored IP address to kernel netdev",
			"interfaceName", state.InterfaceName, "address", addr)
	}
	return nil
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
// device address, applies the scraped IP addresses, and sets up the
// kernel routes needed by FRR.
func configureGroutPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface, pciAddr string, addrs []string) error {
	portName := portName(iface)
	opts := PortOptions{
		RXQueues:    iface.AcceleratedConfig.RXQueues,
		QSize:       iface.AcceleratedConfig.QSize,
		Promiscuous: iface.AcceleratedConfig.Promiscuous,
		MAC:         iface.AcceleratedConfig.MAC,
		Description: UnderlayInterfaceDescriptionMarker,
	}

	if err := client.ensurePortWithOptions(ctx, portName, pciAddr, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
	}

	for _, addr := range addrs {
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
//   - mlx5_core: move the kernel netlink interface to the perouter namespace (bifurcated driver)
//   - vfio-pci: already bound, nothing to do
//   - other kernel drivers: rebind to vfio-pci
func prepareGroutPortDriver(ctx context.Context, perouterNetNS netns.NsHandle, pciAddr, netlinkName string) error {
	driver, err := pci.GetPCIDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to get PCI driver for %s: %w", pciAddr, err)
	}

	switch {
	case pci.IsBifurcated(driver):
		name := netlinkName
		if name == "" {
			name, err = pci.GetPCINetDevice(pciAddr)
			if err != nil {
				return fmt.Errorf("mlx5 PCI device %s has no kernel netlink interface: %w", pciAddr, err)
			}
		}
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, hostnetwork.UnderlayInterface{
			InterfaceName: name,
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
		}); err != nil {
			return fmt.Errorf("failed to move mlx5 netlink device %s to namespace: %w", name, err)
		}
		return nil

	case driver == pci.DriverVFIOPCI:
		return nil

	default:
		if err := pci.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to bind PCI device %s to vfio-pci: %w", pciAddr, err)
		}
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

		// FRR needs kernel routes to establish BGP connections. Grout requires that all the kernel
		// traffic must enter grout via the `main` TAP device.
		if err := ensureKernelSubnetRoute(defaultVRFName, addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", UnderlayPortNamePrefix+underlayInterface)
	}

	// for each port, grout creates a NOARP kernel interface to make FRR zebra daemon work.
	// 5: u_enp3s0: <BROADCAST,MULTICAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
	//    link/ether 00:09:a8:38:8e:3b brd ff:ff:ff:ff:ff:ff promiscuity 0 allmulti 0 minmtu 68 maxmtu 65521
	//    tun type tap ...
	//    alias Grout control plane interface
	// bgpd packets will leave through the `main` interface and will come back on the `u_xxx` interface, hence the
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
		if err := removeKernelSubnetRoute(defaultVRFName, addr); err != nil {
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

func portName(iface hostnetwork.UnderlayInterface) string {
	return UnderlayPortNamePrefix + iface.InterfaceName
}
