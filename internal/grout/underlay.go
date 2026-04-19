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

	if params.UnderlayInterface == "" {
		return nil
	}

	if err := hostnetwork.MoveUnderlayInterface(ctx, params.UnderlayInterface, ns); err != nil {
		return err
	}

	portExists, err := client.portExists(ctx, underlayPortName)
	if err != nil {
		return fmt.Errorf("failed to check grout underlay port: %w", err)
	}
	if portExists {
		slog.InfoContext(ctx, "grout underlay port already exists, skipping", "port", underlayPortName)
		return nil
	}

	var underlayAddrs []netlink.Addr
	if err := netnamespace.In(ns, func() error {
		link, err := netlink.LinkByName(params.UnderlayInterface)
		if err != nil {
			return fmt.Errorf("failed to find underlay interface %s: %w", params.UnderlayInterface, err)
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("failed to list addresses on %s: %w", params.UnderlayInterface, err)
		}
		for _, addr := range addrs {
			if addr.IPNet.String() == hostnetwork.UnderlayInterfaceSpecialAddr {
				continue
			}
			if addr.IP.IsLinkLocalUnicast() || addr.IP.IsLinkLocalMulticast() {
				continue
			}
			underlayAddrs = append(underlayAddrs, addr)
		}
		for i := range underlayAddrs {
			if err := netlink.AddrDel(link, &underlayAddrs[i]); err != nil {
				slog.WarnContext(ctx, "failed to remove address from underlay interface",
					"addr", underlayAddrs[i].IPNet, "error", err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	// Create grout TAP port with remote= so TC ingress rules redirect
	// incoming packets from the physical underlay interface to grout.
	devargs := fmt.Sprintf("net_tap0,remote=%s,iface=%s", params.UnderlayInterface, "tap_"+params.UnderlayInterface)
	if err := client.ensurePort(ctx, underlayPortName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	// Assign IPs to the grout port so FRR (with dplane_grout) discovers
	// the interface and installs a connected route for the underlay subnet.
	// Grout also creates a NOARP kernel interface that the kernel TCP stack
	// uses for BGP sessions — no bridge or kernel-side ARP is needed.
	var ipv4, ipv6 string
	for _, addr := range underlayAddrs {
		cidr := addr.IPNet.String()
		if addr.IP.To4() != nil && ipv4 == "" {
			ipv4 = cidr
		} else if addr.IP.To4() == nil && ipv6 == "" {
			ipv6 = cidr
		}
	}
	if ipv4 != "" || ipv6 != "" {
		if err := assignIPsToGroutPort(ctx, client, underlayPortName, ipv4, ipv6); err != nil {
			return fmt.Errorf("failed to assign IPs to grout underlay port: %w", err)
		}
	}

	return nil
}

// ResolveNeighborARP sends a single grout-level ping to the given address
// to trigger DPDK ARP resolution. Without this, grout cannot properly
// encapsulate packets from the kernel control plane TAP with correct L2
// headers, and the BGP TCP session cannot establish.
func ResolveNeighborARP(ctx context.Context, client *Client, addr string) error {
	slog.InfoContext(ctx, "resolving neighbor ARP via grout ping", "addr", addr)
	if err := client.ping(ctx, addr); err != nil {
		return fmt.Errorf("failed to resolve ARP for %s: %w", addr, err)
	}
	return nil
}
