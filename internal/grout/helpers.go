// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// assignIPsToLink assigns IPv4 and/or IPv6 addresses to a netlink interface.
func assignIPsToLink(link netlink.Link, ipv4, ipv6 string) error {
	if ipv4 == "" && ipv6 == "" {
		return fmt.Errorf("at least one IP address must be provided (IPv4 or IPv6)")
	}

	for _, addr := range []string{ipv4, ipv6} {
		if addr == "" {
			continue
		}
		parsed, err := netlink.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address %s: %w", addr, err)
		}
		if err := netlink.AddrReplace(link, parsed); err != nil {
			return fmt.Errorf("failed to assign address %s to %s: %w", addr, link.Attrs().Name, err)
		}
	}
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
		if err := client.addAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}
	}
	return nil
}

// moveLinkToHostNamespace moves a link from the given namespace to the current
// (host) namespace. If the link is already in the host namespace, this is a no-op.
func moveLinkToHostNamespace(ctx context.Context, name string, srcNS netns.NsHandle) error {
	// Check if already in the host namespace.
	_, err := netlink.LinkByName(name)
	if err == nil {
		slog.DebugContext(ctx, "link already in host namespace", "name", name)
		return nil
	}

	// Get the current (host) namespace fd.
	hostNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to get host namespace: %w", err)
	}
	defer hostNS.Close()

	// Move the link from srcNS to host namespace.
	if err := netnamespace.In(srcNS, func() error {
		link, err := netlink.LinkByName(name)
		if err != nil {
			return fmt.Errorf("link %s not found in source namespace: %w", name, err)
		}
		return netlink.LinkSetNsFd(link, int(hostNS))
	}); err != nil {
		return err
	}

	// Bring it up in the host namespace.
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("link %s not found after move to host: %w", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring %s up: %w", name, err)
	}

	slog.DebugContext(ctx, "link moved to host namespace", "name", name)
	return nil
}

func removeLinkByName(name string) error {
	link, err := netlink.LinkByName(name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove link by name: failed to get link %s: %w", name, err)
	}
	return netlink.LinkDel(link)
}
