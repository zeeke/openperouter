// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/vishvananda/netlink"
)

// setupTAPInHostNamespace creates a kernel TAP interface in the host namespace.
// The TAP device is named after names.HostSide (e.g. "pt-host") and serves as
// the host-side endpoint that connects to the grout port on the dataplane side.
func setupTAPInHostNamespace(ctx context.Context, names hostnetwork.VethNames) error {
	slog.DebugContext(ctx, "setupTAPInHostNamespace", "hostSide", names.HostSide)

	link, err := netlink.LinkByName(names.HostSide)
	if err == nil {
		slog.DebugContext(ctx, "TAP already exists, ensuring it is up", "name", names.HostSide)
		return netlink.LinkSetUp(link)
	}
	if !errors.As(err, &netlink.LinkNotFoundError{}) {
		return fmt.Errorf("checking for existing TAP %s: %w", names.HostSide, err)
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: names.HostSide},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("creating TAP %s: %w", names.HostSide, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("setting TAP %s up: %w", names.HostSide, err)
	}

	slog.DebugContext(ctx, "TAP interface created", "name", names.HostSide)
	return nil
}

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

// assignIPsToGroutPort assigns IPv4 and IPv6 addresses to a grout port by name.
func assignIPsToGroutPort(portName string, ipv4, ipv6 string) error {
	_ = portName
	// TODO: implement IP assignment to grout ports via grcli
	return nil
}
