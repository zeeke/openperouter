// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	underlayPortName = "underlay"
)

func HasUnderlayInterface(ctx context.Context, client *Client) (bool, error) {
	return client.portExists(ctx, underlayPortName)
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
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

	if params.UnderlayInterface != "" {
		if err := hostnetwork.MoveUnderlayInterface(ctx, params.UnderlayInterface, ns); err != nil {
			return err
		}
	}

	// Create a grout TAP port connected to the underlay NIC via the `remote` devarg.
	// This sets up tc rules to forward traffic between the TAP and the kernel NIC.
	devargs := fmt.Sprintf("net_tap0,remote=%s", params.UnderlayInterface)
	if err := client.ensurePort(ctx, underlayPortName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	// Mirror the underlay NIC's IPv4 addresses to the grout port so that
	// FRR/zebra (which only sees grout ports) can resolve BGP neighbors
	// on the underlay network.
	if params.UnderlayInterface != "" {
		if err := mirrorUnderlayAddresses(ctx, client, params.UnderlayInterface, ns); err != nil {
			return fmt.Errorf("failed to mirror underlay addresses to grout port: %w", err)
		}
	}

	return nil
}

// mirrorUnderlayAddresses reads IPv4 addresses from the kernel underlay
// interface and assigns them to the grout underlay port. The special marker
// address used for interface detection is excluded.
func mirrorUnderlayAddresses(ctx context.Context, client *Client, ifaceName string, ns netns.NsHandle) error {
	var addrs []netlink.Addr
	if err := netnamespace.In(ns, func() error {
		link, err := netlink.LinkByName(ifaceName)
		if err != nil {
			return fmt.Errorf("underlay interface %s not found: %w", ifaceName, err)
		}
		var listErr error
		addrs, listErr = netlink.AddrList(link, netlink.FAMILY_V4)
		return listErr
	}); err != nil {
		return err
	}

	for _, addr := range addrs {
		cidr := addr.IPNet.String()
		if cidr == hostnetwork.UnderlayInterfaceSpecialAddr {
			continue
		}
		slog.DebugContext(ctx, "mirroring underlay address to grout port", "addr", cidr)
		if err := client.addAddress(ctx, underlayPortName, cidr); err != nil {
			return fmt.Errorf("failed to assign %s to grout underlay: %w", cidr, err)
		}
	}
	return nil
}
