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
	UnderlayPortNamePrefix             = "u_"
	UnderlayInterfaceDescriptionMarker = "underlay"
	BadConfigurationPortName           = "bad_config"
)

// PortName returns the grout port name for the given underlay interface.
// NetDev/CNIDev → "u_<InterfaceName>"; GroutPort → "p<BDF>" (from
// PCIAddress), netlinkName, or pfName_vfIndex. An explicit
// GroutPort.PortName overrides in all cases.
func PortName(iface hostnetwork.UnderlayInterface) string {
	if iface.GroutPort != nil && iface.GroutPort.PortName != "" {
		return iface.GroutPort.PortName
	}
	switch iface.Kind {
	case hostnetwork.UnderlayInterfaceNetDev, hostnetwork.UnderlayInterfaceCNIDev:
		return UnderlayPortNamePrefix + iface.InterfaceName
	case hostnetwork.UnderlayInterfaceGroutPort:
		if iface.GroutPort == nil {
			return BadConfigurationPortName
		}
		switch {
		case iface.GroutPort.PCIAddress != "":
			return "p" + pciAddressToBDF(iface.GroutPort.PCIAddress)
		case iface.GroutPort.NetlinkName != "":
			return iface.GroutPort.NetlinkName
		case iface.GroutPort.PFName != "" && iface.GroutPort.VFIndex != nil:
			return fmt.Sprintf("%s_%d", iface.GroutPort.PFName, *iface.GroutPort.VFIndex)
		default:
			return BadConfigurationPortName
		}
	default:
		return BadConfigurationPortName
	}
}

func pciAddressToBDF(pciAddr string) string {
	if i := strings.IndexByte(pciAddr, ':'); i >= 0 {
		pciAddr = pciAddr[i+1:]
	}
	return strings.NewReplacer(":", "", ".", "").Replace(pciAddr)
}

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
			if err := setupTapUnderlay(ctx, client, perouterNetNS, params.TargetNS, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := setupTapUnderlay(ctx, client, perouterNetNS, params.TargetNS, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceGroutPort:
			if err := setupGroutPortUnderlay(ctx, client, perouterNetNS, iface); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
		}
	}

	if params.TunnelEndpoint != nil {
		if err := setupTunnelEndpoint(ctx, client, *params.TunnelEndpoint); err != nil {
			return err
		}
	}

	return nil
}

func setupTapUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, targetNS string, iface hostnetwork.UnderlayInterface) error {
	switch iface.Kind {
	case hostnetwork.UnderlayInterfaceNetDev:
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, iface); err != nil {
			return err
		}
	case hostnetwork.UnderlayInterfaceCNIDev:
		if err := hostnetwork.SetupUnderlayCNIDevInterface(ctx, targetNS, iface); err != nil {
			return err
		}
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureUnderlayPort(ctx, client, iface.InterfaceName, PortName(iface))
	})
}

func setupGroutPortUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	if iface.GroutPort == nil {
		return fmt.Errorf("groutPort params missing for interface: %+v", iface)
	}
	if err := resolveGroutPortPCIAddress(iface.GroutPort); err != nil {
		return fmt.Errorf("failed to resolve PCI address for interface: %+v: %w", iface, err)
	}
	if err := scrapeAndSaveDeviceState(ctx, iface.GroutPort.PCIAddress); err != nil {
		return fmt.Errorf("failed to save device state for %s: %w", iface.GroutPort.PCIAddress, err)
	}
	if err := prepareGroutPortDriver(ctx, perouterNetNS, iface.GroutPort.PCIAddress); err != nil {
		return fmt.Errorf("failed to prepare grout port driver for %s: %w", iface.GroutPort.PCIAddress, err)
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureGroutPort(ctx, client, iface)
	})
}

func setupTunnelEndpoint(ctx context.Context, client *Client, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, defaultVRFName,
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	return nil
}

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
		ret = append(ret,
			hostnetwork.UnderlayInterface{
				InterfaceName: groutIface.Name,
				Kind:          hostnetwork.UnderlayInterfaceGroutPort,
				GroutPort: &hostnetwork.GroutPortParams{
					PCIAddress: details.Devargs,
				},
			})
	}
	return ret, nil
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

	for _, iface := range toRemove {
		portName := PortName(iface)
		if _, found := existingPorts[portName]; !found {
			slog.Debug("RestoreUnderlay: port already removed", "namespace", targetNS, "port", portName)
			continue
		}

		switch iface.Kind {
		case hostnetwork.UnderlayInterfaceNetDev:
			if err := teardownTapUnderlay(ctx, client, targetNS, ns, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := teardownTapUnderlay(ctx, client, targetNS, ns, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceGroutPort:
			if err := teardownGroutPortUnderlay(ctx, client, ns, iface); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
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

func teardownGroutPortUnderlay(ctx context.Context, client *Client, ns netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	if err := removeGroutPortAddresses(ctx, client, ns, iface.InterfaceName); err != nil {
		return err
	}

	if err := client.deletePort(ctx, iface.InterfaceName); err != nil {
		slog.ErrorContext(ctx, "failed to delete grout port", "port", PortName(iface), "error", err)
	}

	pciAddr := iface.GroutPort.PCIAddress
	state, err := loadDeviceState(pciAddr)
	if err != nil {
		slog.WarnContext(ctx, "no saved device state, cannot restore driver/IPs",
			"pciAddress", pciAddr, "error", err)
		return nil
	}

	if state.OriginalDriver != "" && state.OriginalDriver != sriov.DriverVFIOPCI {
		if err := sriov.RestoreDriver(pciAddr, state.OriginalDriver); err != nil {
			return fmt.Errorf("failed to restore driver %s on %s: %w",
				state.OriginalDriver, pciAddr, err)
		}
		slog.InfoContext(ctx, "restored original driver",
			"pciAddress", pciAddr, "driver", state.OriginalDriver)
	}

	restoreIPAddresses(ctx, *state)

	if err := deleteDeviceState(pciAddr); err != nil {
		slog.WarnContext(ctx, "failed to delete device state file",
			"pciAddress", pciAddr, "error", err)
	}

	return nil
}

func teardownTapUnderlay(ctx context.Context, client *Client, targetNS string, ns netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	err := netnamespace.In(ns, func() error {
		return migrateAddressesToKernel(ctx, client, PortName(iface), iface.InterfaceName)
	})
	if err != nil {
		return err
	}

	if err := client.deletePort(ctx, PortName(iface)); err != nil {
		slog.ErrorContext(ctx, "failed to delete grout port", "port", PortName(iface), "error", err)
	}

	if err := hostnetwork.RestoreUnderlay(ctx, targetNS, []hostnetwork.UnderlayInterface{iface}); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
	}

	return nil
}

func restoreIPAddresses(ctx context.Context, state SavedDeviceState) {
	if state.NetlinkName == "" || len(state.Addresses) == 0 {
		return
	}
	link, err := netlink.LinkByName(state.NetlinkName)
	if err != nil {
		slog.WarnContext(ctx, "kernel netdev not found after driver restore, cannot re-apply IPs",
			"netlinkName", state.NetlinkName, "error", err)
		return
	}
	for _, addr := range state.Addresses {
		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			slog.ErrorContext(ctx, "failed to restore IP address",
				"address", addr, "netlinkName", state.NetlinkName, "error", err)
		}
	}
	slog.InfoContext(ctx, "restored IP addresses to kernel netdev",
		"netlinkName", state.NetlinkName, "addresses", state.Addresses)
}

func configureUnderlayPort(ctx context.Context, client *Client, underlayInterface, portName string) error {
	underlayAddrs, err := hostnetwork.AddressesForInterface(underlayInterface, hostnetwork.ExcludeLinkLocal())
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	devargs := fmt.Sprintf("net_tap%s,remote=%s,iface=%s", makeTapRandomString(), underlayInterface, "tap_"+underlayInterface)
	if err := client.ensurePort(ctx, portName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	// Disable SLAAC on the underlying netlink interface: once the intended
	// addresses are migrated to the grout port, the kernel must not
	// autoconfigure new ones from Router Advertisements. A SLAAC address
	// on the netlink device would be picked up on the next reconcile and
	// clash with the existing connected route in grout's RIB (EBUSY).
	if err := sysctl.Ensure(sysctl.DisableAcceptRA(underlayInterface)); err != nil {
		return fmt.Errorf("failed to disable accept_ra on underlay interface %s: %w", underlayInterface, err)
	}

	if err := migrateAddressesToGrout(ctx, client, underlayInterface, portName, underlayAddrs); err != nil {
		return err
	}

	return nil
}

// resolveGroutPortPCIAddress resolves the PCI address from whichever
// device selector was set on the GroutPortParams. This runs on the node
// where sysfs is available, not in the controller.
func resolveGroutPortPCIAddress(params *hostnetwork.GroutPortParams) error {
	switch {
	case params.NetlinkName != "":
		pciAddr, err := sriov.ResolveNetlinkName(params.NetlinkName)
		if err != nil {
			return err
		}
		params.PCIAddress = pciAddr
	case params.PFName != "" && params.VFIndex != nil:
		pciAddr, err := sriov.ResolvePFVFIndex(params.PFName, int(*params.VFIndex))
		if err != nil {
			return err
		}
		params.PCIAddress = pciAddr
	case params.PCIAddress != "":
		if err := sriov.ResolvePCIAddress(params.PCIAddress); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no device selector set (need pciAddress, pfName+vfIndex, or netlinkName)")
	}
	return nil
}

// scrapeAndSaveDeviceState reads the kernel netdev name, IP addresses,
// and current driver from a PCI device before the DPDK driver is bound,
// and persists them to a state file for teardown restoration.
func scrapeAndSaveDeviceState(ctx context.Context, pciAddr string) error {
	netlinkName, err := sriov.GetPCINetDevice(pciAddr)
	if err != nil {
		slog.WarnContext(ctx, "no kernel netdev for PCI device, saving state without netlink name",
			"pciAddress", pciAddr, "error", err)
		netlinkName = ""
	}

	var addrs []string
	if netlinkName != "" {
		netlinkAddrs, err := hostnetwork.AddressesForInterface(netlinkName, hostnetwork.ExcludeLinkLocal())
		if err != nil {
			return fmt.Errorf("failed to read addresses from %s: %w", netlinkName, err)
		}
		for _, a := range netlinkAddrs {
			addrs = append(addrs, a.IPNet.String())
		}
	}

	driver, err := sriov.GetPCIDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to read driver for %s: %w", pciAddr, err)
	}

	state := SavedDeviceState{
		PCIAddress:     pciAddr,
		NetlinkName:    netlinkName,
		OriginalDriver: driver,
		Addresses:      addrs,
	}
	if err := saveDeviceState(state); err != nil {
		return err
	}

	slog.InfoContext(ctx, "saved device state before DPDK binding",
		"pciAddress", pciAddr, "netlinkName", netlinkName,
		"driver", driver, "addresses", addrs)
	return nil
}

// configureGroutPort creates a DPDK port in grout directly from a PCI
// device address, loads the scraped IP addresses from the saved device
// state, and sets up the kernel routes needed by FRR.
func configureGroutPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface) error {
	if iface.GroutPort == nil {
		return fmt.Errorf("groutPort params missing for interface %s", iface.InterfaceName)
	}

	portName := PortName(iface)
	opts := PortOptions{
		MTU:      iface.GroutPort.MTU,
		RXQueues: iface.GroutPort.RXQueues,
		QSize:    iface.GroutPort.QSize,
	}

	if err := client.ensurePortWithOptions(ctx, portName, iface.GroutPort.PCIAddress, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
	}

	state, err := loadDeviceState(iface.GroutPort.PCIAddress)
	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", iface.GroutPort.PCIAddress, err)
	}

	for _, addr := range state.Addresses {
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
//   - unknown/unbound: bind to vfio-pci
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
		slog.Info("binding GroutPort PCI device to vfio-pci",
			"pciAddress", pciAddr, "currentDriver", driver)
		if err := sriov.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to bind PCI device %s to vfio-pci: %w", pciAddr, err)
		}
		return nil
	}
}

// PrepareAndBindTrunkVF prepares the DPDK driver for a trunk VF and creates
// the grout port. It is idempotent: calling it for a PCI address that is
// already bound is a no-op.
func PrepareAndBindTrunkVF(ctx context.Context, client *Client, targetNS, pciAddr, portName string, opts PortOptions) error {
	perouterNetNS, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := perouterNetNS.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()
	if err := prepareGroutPortDriver(ctx, perouterNetNS, pciAddr); err != nil {
		return fmt.Errorf("failed to prepare trunk VF driver for %s: %w", pciAddr, err)
	}

	return client.ensurePortWithOptions(ctx, portName, pciAddr, opts)
}

func migrateAddressesToGrout(ctx context.Context, client *Client, kernelDevice, portName string, addrs []netlink.Addr) error {
	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		if err := client.ensureAddress(ctx, portName, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout underlay port: %w", cidr, err)
		}

		if err := hostnetwork.DeleteAddressFromInterface(kernelDevice, addr); err != nil {
			return fmt.Errorf("failed to remove address %s from underlay interface: %w", cidr, err)
		}

		if err := ensureKernelSubnetRoute(defaultVRFName, addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", portName)
	}

	if err := sysctl.Ensure(sysctl.DisableRPFilter(portName)); err != nil {
		return fmt.Errorf("failed to disable rp_filter on underlay interface %s: %w", portName, err)
	}

	return nil
}

func migrateAddressesToKernel(ctx context.Context, client *Client, underlayPortName, netlinkName string) error {
	addrs, err := client.getAddresses(ctx, underlayPortName)
	if err != nil {
		return fmt.Errorf("failed to read addresses from grout port %s: %w", underlayPortName, err)
	}
	if len(addrs) == 0 {
		return nil
	}

	link, err := netlink.LinkByName(netlinkName)
	if err != nil {
		return fmt.Errorf("failed to find link %s: %w", netlinkName, err)
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
			return fmt.Errorf("failed to assign address %s to link %s: %w", addr, netlinkName, err)
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
