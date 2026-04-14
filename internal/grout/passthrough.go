// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// SetupPassthrough configures the passthrough interface via the grout dataplane.
// It creates a grout port "pt-ns" with a kernel TAP "pt-host", moves the TAP
// to the host namespace, and assigns IPs to both sides.
func SetupPassthrough(ctx context.Context, client *Client, params hostnetwork.PassthroughParams) error {
	slog.DebugContext(ctx, "setup passthrough", "params", params)
	defer slog.DebugContext(ctx, "setup passthrough done")

	tapName := hostnetwork.PassthroughNames.HostSide       // "pt-host"
	portName := hostnetwork.PassthroughNames.NamespaceSide  // "pt-ns"

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupPassthrough: failed to find namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	// Check if the grout port already exists. If it does, the TAP was already
	// moved to the host namespace on a previous reconciliation — skip creation.
	portExists, err := client.portExists(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to check grout port %s: %w", portName, err)
	}

	if !portExists {
		// Remove any stale TAP from a previous run in the host namespace.
		_ = removeLinkByName(tapName)

		// Create grout port "pt-ns" with TAP "pt-host" in the router pod's namespace.
		devargs := fmt.Sprintf("net_tap1,iface=%s", tapName)
		if err := client.ensurePort(ctx, portName, devargs); err != nil {
			return fmt.Errorf("failed to create grout passthrough port: %w", err)
		}

		// Set a different MAC on the TAP to avoid Linux from wrongfully assuming
		// that packets sent by grout originated locally.
		if err := netnamespace.In(ns, func() error {
			tap, err := netlink.LinkByName(tapName)
			if err != nil {
				return fmt.Errorf("TAP %s not found in namespace after creation: %w", tapName, err)
			}
			mac := deterministicMAC(tapName)
			return netlink.LinkSetHardwareAddr(tap, mac)
		}); err != nil {
			return fmt.Errorf("failed to set TAP MAC: %w", err)
		}

		// Move TAP "pt-host" from the router pod's namespace to the host namespace
		// so that FRR-K8s (running on the host) can peer with it.
		if err := moveLinkToHostNamespace(ctx, tapName, ns); err != nil {
			return fmt.Errorf("failed to move TAP to host namespace: %w", err)
		}
	} else {
		slog.InfoContext(ctx, "grout passthrough port already exists, skipping TAP setup", "port", portName)
	}

	// Assign IPs to the host-side TAP (now in host namespace).
	hostTap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("host TAP %s not found after move: %w", tapName, err)
	}
	if err := assignIPsToLink(hostTap, params.HostVeth.HostIPv4, params.HostVeth.HostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to host TAP: %w", err)
	}

	// Assign IPs to the grout port "pt-ns" via grcli.
	if err := assignIPsToGroutPort(ctx, client, portName, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	return nil
}

func RemovePassthrough(ctx context.Context, client *Client, targetNS string) error {
	// Remove the host-side TAP if it exists.
	if err := removeLinkByName(hostnetwork.PassthroughNames.HostSide); err != nil {
		return fmt.Errorf("RemovePassthrough: failed to remove host TAP %s: %w", hostnetwork.PassthroughNames.HostSide, err)
	}
	// Delete the grout port so it can be recreated on the next setup.
	portName := hostnetwork.PassthroughNames.NamespaceSide
	if err := client.deletePort(ctx, portName); err != nil {
		return fmt.Errorf("RemovePassthrough: failed to delete grout port %s: %w", portName, err)
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

// deterministicMAC generates a locally-administered MAC address from the given name.
func deterministicMAC(name string) []byte {
	h := md5.Sum([]byte(name))
	return []byte{0x02, h[0], h[1], h[2], h[3], h[4]}
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
