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
	"github.com/openperouter/openperouter/internal/sysctl"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	UnderlayPortNamePrefix             = "u_"
	UnderlayInterfaceDescriptionPrefix = "underlay-for="
)

// PortName returns the grout port name for the given underlay interface.
// An AcceleratedConfig PortName override takes precedence over "u_<InterfaceName>".
func PortName(iface hostnetwork.UnderlayInterface) string {
	if iface.AcceleratedConfig != nil && iface.AcceleratedConfig.PortName != nil {
		return *iface.AcceleratedConfig.PortName
	}
	return UnderlayPortNamePrefix + iface.InterfaceName
}

// SetupUnderlay configures the underlay interfaces via the grout dataplane.
// tapUnderlay selects TAP+remote= ports; otherwise kernel netdevs are bound
// directly as PCI ports. Grout owns the underlay addresses and forwarding.
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams, tapUnderlay bool) error {
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
	if toRemove := underlayInterfacesToRemove(existing, params.UnderlayInterfaces); len(toRemove) > 0 {
		slog.InfoContext(ctx, "underlay interfaces changed, removing old interfaces before setup",
			"toRemove", toRemove, "requested", params.UnderlayInterfaces)
		if err := RestoreUnderlay(ctx, client, params.TargetNS, toRemove); err != nil {
			return fmt.Errorf("failed to remove old underlay interfaces: %w", err)
		}
	}

	for _, iface := range params.UnderlayInterfaces {
		if err := setupUnderlayInterface(ctx, client, perouterNetNS, iface, tapUnderlay); err != nil {
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

// UnderlayInterfaces returns the netdev underlays configured in grout, in the
// saved device state and in the namespace, deduplicated by interface name.
// The device state makes an underlay whose grout port is already gone
// discoverable, so an interrupted teardown is resumed on the next reconcile.
// CNI dev underlays are not supported by the grout datapath, so any libcni
// cache entry is ignored.
func UnderlayInterfaces(ctx context.Context, client *Client, namespace string) ([]hostnetwork.UnderlayInterface, error) {
	fromGrout, err := groutUnderlayInterfaces(ctx, client)
	if err != nil {
		return nil, err
	}
	fromState, err := savedUnderlayInterfaces()
	if err != nil {
		return nil, err
	}
	fromHost, err := hostUnderlayInterfaces(namespace)
	if err != nil {
		return nil, err
	}
	return mergeUnderlayInterfaces(fromGrout, fromState, fromHost), nil
}

// RestoreUnderlay removes the given underlay interfaces: it tears down their
// grout ports first (migrating the underlay addresses back to the kernel
// interfaces), then hands the devices back to the kernel as described by
// their saved device state. Every step is idempotent and the device state is
// deleted last, so a failed teardown is resumed by the next call.
func RestoreUnderlay(
	ctx context.Context,
	client *Client,
	targetNS string,
	toRemove []hostnetwork.UnderlayInterface,
) error {
	if len(toRemove) == 0 {
		return nil
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
		if err := teardownUnderlayInterface(ctx, client, ns, targetNS, iface); err != nil {
			return err
		}
	}

	return nil
}

func setupUnderlayInterface(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, iface hostnetwork.UnderlayInterface, tapUnderlay bool) error {
	if iface.Kind == hostnetwork.UnderlayInterfaceNetDev && tapUnderlay {
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, iface); err != nil {
			return err
		}
		return netnamespace.In(perouterNetNS, func() error {
			return configureUnderlayGroutTapPort(ctx, client, iface)
		})
	}

	if iface.Kind == hostnetwork.UnderlayInterfaceNetDev {
		return setupAcceleratedUnderlay(ctx, client, perouterNetNS, iface)
	}

	return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
}

func setupTunnelEndpoint(ctx context.Context, client *Client, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, defaultVRFName,
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	return nil
}

// groutPortToUnderlayInterface returns the host underlay interface a grout port
// was created for, read from the port description. It reports false for
// ports that are not underlays.
func groutPortToUnderlayInterface(
	interfaceProperties *groutInterfaceProperties,
) (hostnetwork.UnderlayInterface, bool, error) {
	interfaceName, found := strings.CutPrefix(interfaceProperties.Description, UnderlayInterfaceDescriptionPrefix)
	if !found {
		return hostnetwork.UnderlayInterface{}, false, nil
	}
	if interfaceName == "" {
		return hostnetwork.UnderlayInterface{}, false,
			fmt.Errorf("grout underlay port %s has no interface name", interfaceProperties.Name)
	}
	portName := interfaceProperties.Name
	return hostnetwork.UnderlayInterface{
		InterfaceName:     interfaceName,
		Kind:              hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{PortName: &portName},
	}, true, nil
}

func removeAddressFromGroutPort(ctx context.Context, client *Client, ns netns.NsHandle, portName string) error {
	return netnamespace.In(ns, func() error {
		addrs, err := client.getAddresses(ctx, portName)
		if err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to get addresses for grout port %s: %w", portName, err)
		}
		for _, addr := range addrs {
			if err := removeKernelSubnetRoute(defaultVRFName, addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to remove kernel route for %s: %w", addr, err)
			}
			if err := client.deleteAddress(ctx, portName, addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to delete address %s from grout port %s: %w", addr, portName, err)
			}
		}
		return nil
	})
}

func teardownUnderlayInterface(
	ctx context.Context,
	client *Client,
	ns netns.NsHandle,
	targetNS string,
	iface hostnetwork.UnderlayInterface,
) error {
	if iface.Kind != hostnetwork.UnderlayInterfaceNetDev {
		return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
	}

	// The port must be gone before the device is handed back to the kernel:
	// DPDK may still own it.
	if err := deleteUnderlayPort(ctx, client, ns, PortName(iface)); err != nil {
		return err
	}

	state, err := devicestate.Load(iface.InterfaceName)
	if errors.Is(err, devicestate.ErrDeviceStateNotFound) {
		slog.WarnContext(ctx, "no saved device state, cannot restore driver/IPs", "interfaceName", iface.InterfaceName)
		state, err = &devicestate.Entry{InterfaceName: iface.InterfaceName}, nil
	}
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to load device state for %s: %w", iface.InterfaceName, err)
	}

	if state.PCIAddress != "" {
		err = restoreAcceleratedDevice(ctx, targetNS, state)
	} else {
		err = restoreTapDevice(ctx, targetNS, state)
	}
	if err != nil {
		return err
	}

	if err := devicestate.Delete(iface.InterfaceName); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to delete device state for %s: %w", iface.InterfaceName, err)
	}
	return nil
}

func deleteUnderlayPort(ctx context.Context, client *Client, ns netns.NsHandle, portName string) error {
	exists, err := client.portExists(ctx, portName)
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to inspect grout port %s: %w", portName, err)
	}
	if !exists {
		return nil
	}
	if err := removeAddressFromGroutPort(ctx, client, ns, portName); err != nil {
		return err
	}
	if err := client.deletePort(ctx, portName); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to delete grout port %s: %w", portName, err)
	}
	return nil
}

// restoreTapDevice moves the netdev backing a TAP port back to the host
// namespace and re-applies its saved MTU and addresses. A netdev that no
// longer exists is skipped, as there is nothing left to restore.
func restoreTapDevice(ctx context.Context, targetNS string, state *devicestate.Entry) error {
	if err := hostnetwork.RestoreUnderlay(ctx, targetNS, []hostnetwork.UnderlayInterface{
		{InterfaceName: state.InterfaceName, Kind: hostnetwork.UnderlayInterfaceNetDev},
	}); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
	}

	link, err := netlink.LinkByName(state.InterfaceName)
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		slog.WarnContext(ctx, "kernel netdev is gone, nothing to restore", "interfaceName", state.InterfaceName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to find kernel netdev %s: %w", state.InterfaceName, err)
	}
	return applyStateToLink(ctx, link, state)
}

func applyStateToLink(ctx context.Context, link netlink.Link, state *devicestate.Entry) error {
	name := link.Attrs().Name
	if state.MTU > 0 {
		if err := netlink.LinkSetMTU(link, int(state.MTU)); err != nil {
			return fmt.Errorf("failed to restore MTU %d to kernel netdev [%s]: %w", state.MTU, name, err)
		}
	}
	for _, addr := range state.Addresses {
		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			return fmt.Errorf("failed to restore IP address %s to kernel netdev [%s]: %w", addr, name, err)
		}
		slog.InfoContext(ctx, "restored IP addresses to kernel netdev",
			"interfaceName", name, "addresses", addr)
	}
	return nil
}

func configureUnderlayGroutTapPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface) error {
	underlayInterface := iface.InterfaceName
	portName := PortName(iface)
	devState, err := devicestate.Load(underlayInterface)
	if errors.Is(err, devicestate.ErrDeviceStateNotFound) {
		devState, err = deviceStateForTapDevice(underlayInterface)
		if err != nil {
			return err
		}
		devState.PortName = portName
		if err := devicestate.Save(underlayInterface, *devState); err != nil {
			return fmt.Errorf("failed to save device state for %s: %w", underlayInterface, err)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", underlayInterface, err)
	}

	// Suppress the veth's kernel IPv6 link-local so it doesn't collide with the
	// identical EUI-64 link-local on grout's shadow (u_<iface>, which shares the
	// veth's MAC). The collision makes DAD strip the shadow's link-local,
	// breaking any session with an IPv6 nexthop. The veth's link-local is unused
	// since grout owns all forwarding.
	slog.InfoContext(ctx, "suppressing kernel link-local on underlay interface", "iface", underlayInterface)
	if err := hostnetwork.SuppressLinkLocal(underlayInterface); err != nil {
		return fmt.Errorf("failed to suppress link-local on underlay interface %s: %w", underlayInterface, err)
	}

	devargs := fmt.Sprintf("net_tap%s,remote=%s,iface=%s", makeTapRandomString(), underlayInterface, "tap_"+underlayInterface)
	opts := underlayPortOptions(iface)
	if err := client.ensurePortWithOptions(ctx, portName, devargs, opts); err != nil {
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

	underlayAddrs, err := parseAddresses(devState.Addresses)
	if err != nil {
		return err
	}

	if err := migrateAddressesToGrout(ctx, client, underlayInterface, portName, underlayAddrs); err != nil {
		return err
	}

	return nil
}

func groutUnderlayInterfaces(ctx context.Context, client *Client) ([]hostnetwork.UnderlayInterface, error) {
	groutInterfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return nil, err
	}

	var ret []hostnetwork.UnderlayInterface
	for _, groutIface := range groutInterfaces {
		interfaceProperties, err := client.getInterfaceDetails(ctx, groutIface.Name)
		if err != nil {
			return nil, err
		}
		iface, isUnderlay, err := groutPortToUnderlayInterface(interfaceProperties)
		if err != nil {
			return nil, err
		}
		if !isUnderlay {
			continue
		}
		ret = append(ret, iface)
	}
	return ret, nil
}

func savedUnderlayInterfaces() ([]hostnetwork.UnderlayInterface, error) {
	states, err := devicestate.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list saved device states: %w", err)
	}
	ret := make([]hostnetwork.UnderlayInterface, 0, len(states))
	for _, state := range states {
		ret = append(ret, deviceStateToUnderlayInterface(state))
	}
	return ret, nil
}

func hostUnderlayInterfaces(namespace string) ([]hostnetwork.UnderlayInterface, error) {
	hostInterfaces, err := hostnetwork.UnderlayInterfaces(namespace)
	if err != nil {
		return nil, err
	}
	var ret []hostnetwork.UnderlayInterface
	for _, iface := range hostInterfaces {
		if iface.Kind == hostnetwork.UnderlayInterfaceNetDev {
			ret = append(ret, iface)
		}
	}
	return ret, nil
}

func deviceStateToUnderlayInterface(state devicestate.Entry) hostnetwork.UnderlayInterface {
	iface := hostnetwork.UnderlayInterface{
		InterfaceName: state.InterfaceName,
		Kind:          hostnetwork.UnderlayInterfaceNetDev,
	}
	if state.PortName != "" {
		iface.AcceleratedConfig = &hostnetwork.AcceleratedConfigParams{PortName: &state.PortName}
	}
	return iface
}

// mergeUnderlayInterfaces concatenates the given lists, keeping only the
// first occurrence of each interface name.
func mergeUnderlayInterfaces(lists ...[]hostnetwork.UnderlayInterface) []hostnetwork.UnderlayInterface {
	seen := map[string]bool{}
	var ret []hostnetwork.UnderlayInterface
	for _, list := range lists {
		for _, iface := range list {
			if seen[iface.InterfaceName] {
				continue
			}
			seen[iface.InterfaceName] = true
			ret = append(ret, iface)
		}
	}
	return ret
}

func underlayInterfacesToRemove(existing, requested []hostnetwork.UnderlayInterface) []hostnetwork.UnderlayInterface {
	requestedByName := make(map[string]hostnetwork.UnderlayInterface, len(requested))
	for _, iface := range requested {
		requestedByName[iface.InterfaceName] = iface
	}
	var removed []hostnetwork.UnderlayInterface
	for _, iface := range existing {
		req, found := requestedByName[iface.InterfaceName]
		if !found || req.Kind != iface.Kind || PortName(iface) != PortName(req) {
			removed = append(removed, iface)
		}
	}
	return removed
}

func underlayPortOptions(iface hostnetwork.UnderlayInterface) PortOptions {
	opts := PortOptions{Description: UnderlayInterfaceDescriptionPrefix + iface.InterfaceName}
	if iface.AcceleratedConfig != nil {
		opts.RXQueues = iface.AcceleratedConfig.RXQueues
		opts.QSize = iface.AcceleratedConfig.QSize
	}
	return opts
}

func deviceStateForTapDevice(netlinkName string) (*devicestate.Entry, error) {
	devState := devicestate.Entry{
		InterfaceName: netlinkName,
	}
	var err error
	link, err := netlink.LinkByName(netlinkName)
	if err != nil {
		return nil, fmt.Errorf("failed to find kernel interface %s: %w", netlinkName, err)
	}
	devState.MTU = int32(link.Attrs().MTU)

	netlinkAddrs, err := hostnetwork.AddressesForInterface(netlinkName, hostnetwork.ExcludeLinkLocal())
	if err != nil {
		return nil, fmt.Errorf("failed to read addresses from %s: %w", netlinkName, err)
	}
	for _, a := range netlinkAddrs {
		devState.Addresses = append(devState.Addresses, a.IPNet.String())
	}
	return &devState, nil
}

func migrateAddressesToGrout(ctx context.Context, client *Client, kernelDevice, portName string, addrs []netlink.Addr) error {
	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		if err := client.ensureAddress(ctx, portName, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout underlay port: %w", cidr, err)
		}

		if err := hostnetwork.DeleteAddressFromInterface(kernelDevice, addr); err != nil {
			slog.WarnContext(ctx, "failed to remove address from underlay interface", "cidr", cidr, "iface", kernelDevice, "error", err)
		}

		// FRR needs kernel routes to establish BGP connections. Grout requires that all the kernel
		// traffic must enter grout via the `main` TAP device.
		if err := ensureKernelSubnetRoute(defaultVRFName, addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", portName)
	}

	return nil
}

func parseAddresses(addrs []string) ([]netlink.Addr, error) {
	ret := make([]netlink.Addr, 0, len(addrs))
	for _, addr := range addrs {
		parsed, err := netlink.ParseAddr(addr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse saved address %s: %w", addr, err)
		}
		ret = append(ret, *parsed)
	}
	return ret, nil
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
