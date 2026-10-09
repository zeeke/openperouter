// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/openperouter/openperouter/internal/pci"
	"github.com/openperouter/openperouter/internal/sysctl"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	netdevPollInterval = 100 * time.Millisecond
	netdevPollTimeout  = 10 * time.Second
)

func setupAcceleratedUnderlay(
	ctx context.Context,
	client *Client,
	perouterNetNS netns.NsHandle,
	iface hostnetwork.UnderlayInterface,
) error {
	devState, err := devicestate.Load(iface.InterfaceName)
	if errors.Is(err, devicestate.ErrDeviceStateNotFound) {
		devState, err = deviceStateForAcceleratedDevice(iface.InterfaceName)
		if err != nil {
			return err
		}
		devState.PortName = PortName(iface)
		if err := devicestate.Save(iface.InterfaceName, *devState); err != nil {
			return fmt.Errorf("failed to save device state for %s: %w", iface.InterfaceName, err)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", iface.InterfaceName, err)
	}
	// A state file left by the TAP path has no PCI address: going on would
	// poke at bogus sysfs paths.
	if devState.PCIAddress == "" {
		return fmt.Errorf("device state for %s has no PCI address, it was not saved for an accelerated port",
			iface.InterfaceName)
	}

	err = setupInterfaceForHWAcceleration(ctx, perouterNetNS, devState.PCIAddress, devState.InterfaceName)
	if err != nil {
		return fmt.Errorf("failed to prepare grout port driver for %s: %w", devState.PCIAddress, err)
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureAcceleratedPort(ctx, client, iface, devState)
	})
}

// setupInterfaceForHWAcceleration inspects the driver bound to a PCI device and
// takes the appropriate action:
//   - vfio-pci: already bound, nothing to do
//   - mlx5_core: move the kernel netdev to the perouter namespace (bifurcated driver)
//   - any other driver, or none: bind to vfio-pci
func setupInterfaceForHWAcceleration(ctx context.Context, perouterNetNS netns.NsHandle, pciAddr, netdev string) error {
	driver, err := pci.DriverForPCIAddress(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to get PCI driver for %s: %w", pciAddr, err)
	}

	switch driver {
	case pci.DriverVFIOPCI:
		return nil

	case pci.DriverMlx5Core:
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, hostnetwork.UnderlayInterface{
			InterfaceName: netdev,
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
		}); err != nil {
			return fmt.Errorf("failed to move mlx5 netlink device %s to namespace: %w", netdev, err)
		}
		return nil

	default:
		slog.InfoContext(ctx, "binding GroutPort PCI device to vfio-pci",
			"pciAddress", pciAddr, "currentDriver", driver)
		if err := pci.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to bind PCI device %s to vfio-pci: %w", pciAddr, err)
		}
		return nil
	}
}

// configureAcceleratedPort creates a DPDK port in grout directly from a PCI
// device address, loads the scraped IP addresses from the saved device
// state, and sets up the kernel routes needed by FRR.
func configureAcceleratedPort(
	ctx context.Context,
	client *Client,
	iface hostnetwork.UnderlayInterface,
	state *devicestate.Entry,
) error {
	portName := PortName(iface)

	opts := underlayportOptions(iface)
	opts.MTU = &state.MTU

	if err := client.ensurePort(ctx, portName, state.PCIAddress, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
	}

	if pci.IsBifurcated(state.OriginalDriver) {
		return migrateBifurcatedAddresses(ctx, client, state, portName)
	}

	for _, addr := range state.Addresses {
		if err := client.ensureAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}

		if err := ensureKernelSubnetRoute(defaultVRFName, addr); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "configured grout DPDK port address", "cidr", addr, "port", portName)
	}

	return nil
}

// migrateBifurcatedAddresses moves the addresses of a bifurcated device from
// its kernel netdev to the grout port. The netdev lives on in the router
// namespace next to the DPDK port, so, as on the TAP path, it must give up
// its addresses and link-local and stop autoconfiguring new ones from Router
// Advertisements: otherwise they clash with grout's connected routes (EBUSY)
// and with the identical link-local of the port sharing its MAC.
func migrateBifurcatedAddresses(ctx context.Context, client *Client, state *devicestate.Entry, portName string) error {
	netdev := state.InterfaceName
	if err := hostnetwork.SuppressLinkLocal(netdev); err != nil {
		return fmt.Errorf("failed to suppress link-local on bifurcated netdev %s: %w", netdev, err)
	}
	if err := sysctl.Ensure(sysctl.DisableAcceptRA(netdev)); err != nil {
		return fmt.Errorf("failed to disable accept_ra on bifurcated netdev %s: %w", netdev, err)
	}
	addrs, err := parseAddresses(state.Addresses)
	if err != nil {
		return err
	}
	return migrateAddressesToGrout(ctx, client, netdev, portName, addrs)
}

func deviceStateForAcceleratedDevice(netlinkName string) (*devicestate.Entry, error) {
	pciAddr, err := pci.GetPCIAddressForNetlinkName(netlinkName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PCI address for %s: %w", netlinkName, err)
	}
	driver, err := pci.DriverForPCIAddress(pciAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to read driver for %s: %w", pciAddr, err)
	}
	devState := devicestate.Entry{
		InterfaceName:  netlinkName,
		PCIAddress:     pciAddr,
		OriginalDriver: driver,
	}

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

// restoreAcceleratedDevice hands a PCI device back to its original kernel
// driver and re-applies the saved MTU and addresses to its netdev.
func restoreAcceleratedDevice(ctx context.Context, targetNS string, state *devicestate.Entry) error {
	switch {
	case state.OriginalDriver == "" || state.OriginalDriver == pci.DriverVFIOPCI:
		return nil
	case pci.IsBifurcated(state.OriginalDriver):
		return restoreBifurcatedDevice(ctx, targetNS, state)
	}

	if err := pci.RestoreDriver(state.PCIAddress, state.OriginalDriver); err != nil {
		return fmt.Errorf("failed to restore driver %s on %s: %w", state.OriginalDriver, state.PCIAddress, err)
	}
	slog.InfoContext(ctx, "restored original driver", "pciAddress", state.PCIAddress, "driver", state.OriginalDriver)

	link, err := waitForPCINetdev(ctx, state.PCIAddress)
	if err != nil {
		return err
	}
	return applyStateToLink(ctx, link, state)
}

func restoreBifurcatedDevice(ctx context.Context, targetNS string, state *devicestate.Entry) error {
	if err := hostnetwork.RestoreUnderlayNetDevInterface(ctx, targetNS, state.InterfaceName); err != nil {
		return fmt.Errorf("failed to move bifurcated netdev %s back to the host namespace: %w",
			state.InterfaceName, err)
	}
	link, err := netlink.LinkByName(state.InterfaceName)
	if err != nil {
		return fmt.Errorf("failed to find kernel netdev %s: %w", state.InterfaceName, err)
	}
	return applyStateToLink(ctx, link, state)
}

// waitForPCINetdev returns the netdev of a PCI device just rebound to its
// kernel driver. The netdev appears asynchronously and udev may rename it, so
// it is looked up by PCI address until it shows up.
func waitForPCINetdev(ctx context.Context, pciAddr string) (netlink.Link, error) {
	var link netlink.Link
	err := wait.PollUntilContextTimeout(ctx, netdevPollInterval, netdevPollTimeout, true,
		func(context.Context) (bool, error) {
			name, err := pci.NetDeviceForPCIAddress(pciAddr)
			if err != nil {
				return false, nil
			}
			link, err = netlink.LinkByName(name)
			return err == nil, nil
		})
	if err != nil {
		return nil, fmt.Errorf("kernel netdev for PCI device %s did not appear: %w", pciAddr, err)
	}
	return link, nil
}
