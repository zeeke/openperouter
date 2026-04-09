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

func SetupPassthrough(ctx context.Context, params hostnetwork.PassthroughParams) error {
	slog.DebugContext(ctx, "setup passthrough", "params", params)
	defer slog.DebugContext(ctx, "setup passthrough done")

	// Create the host-side TAP interface (e.g. "pt-host").
	if err := setupTAPInHostNamespace(ctx, hostnetwork.PassthroughNames); err != nil {
		return fmt.Errorf("SetupPassthrough: failed to setup TAP interface: %w", err)
	}

	// Assign IPs to the host-side TAP.
	hostTap, err := netlink.LinkByName(hostnetwork.PassthroughNames.HostSide)
	if err != nil {
		return fmt.Errorf("SetupPassthrough: host tap %s not found: %w", hostnetwork.PassthroughNames.HostSide, err)
	}
	if err := assignIPsToLink(hostTap, params.HostVeth.HostIPv4, params.HostVeth.HostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to host tap: %w", err)
	}

	// Assign IPs to the namespace-side grout port (e.g. "pt-ns").
	if err := assignIPsToGroutPort(hostnetwork.PassthroughNames.NamespaceSide, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	return nil
}

func RemovePassthrough(targetNS string) error {
	// TODO: implement grout-based passthrough removal
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
