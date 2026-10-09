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
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	netdevPollInterval = 100 * time.Millisecond
	netdevPollTimeout  = 10 * time.Second
)

func setupAcceleratedUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
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

	if err := setupInterfaceForHWAcceleration(ctx, perouterNetNS, devState.PCIAddress, devState.InterfaceName); err != nil {
		return fmt.Errorf("failed to prepare grout port driver for %s: %w", devState.PCIAddress, err)
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureAcceleratedPort(ctx, client, iface, devState)
	})
}

// setupInterfaceForHWAcceleration inspects the driver bound to a PCI device and
// takes the appropriate action:
//   - Intel kernel drivers (igb, iavf, ice, i40e): rebind to vfio-pci
//   - vfio-pci: already bound, nothing to do
//   - mlx5_core: move the kernel netlink interface to the perouter namespace (bifurcated driver)
//   - unknown/unbound: bind to vfio-pci
func setupInterfaceForHWAcceleration(ctx context.Context, perouterNetNS netns.NsHandle, pciAddr, netlinkName string) error {
	driver, err := pci.DriverForPCAddress(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to get PCI driver for %s: %w", pciAddr, err)
	}

	switch driver {
	case pci.DriverVFIOPCI:
		return nil

	case pci.DriverMlx5Core:
		name := netlinkName
		if name == "" {
			name, err = pci.NetDeviceForPCIAddress(pciAddr)
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

	default:
		slog.Info("binding GroutPort PCI device to vfio-pci",
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
func configureAcceleratedPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface, state *devicestate.Entry) error {
	portName := PortName(iface)

	opts := underlayPortOptions(iface)
	opts.MTU = &state.MTU

	if err := client.ensurePortWithOptions(ctx, portName, state.PCIAddress, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
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

func deviceStateForAcceleratedDevice(netlinkName string) (*devicestate.Entry, error) {
	devState := devicestate.Entry{
		InterfaceName: netlinkName,
	}
	var err error
	devState.PCIAddress, err = pci.GetPCIAddressForNetlinkName(netlinkName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve PCI address for %s: %w", netlinkName, err)
	}
	devState.OriginalDriver, err = pci.DriverForPCAddress(devState.PCIAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to read driver for %s: %w", devState.PCIAddress, err)
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
