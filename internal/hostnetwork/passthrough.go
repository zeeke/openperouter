// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

type PassthroughParams struct {
	TargetNS string `json:"namespace"`
	HostVeth Veth   `json:"veth"`
}

var PassthroughNames = VethNames{
	HostSide:      "pt-host",
	NamespaceSide: "pt-ns",
}

func SetupPassthrough(ctx context.Context, params PassthroughParams) error {
	slog.DebugContext(ctx, "setup passthrough", "params", params)
	defer slog.DebugContext(ctx, "setup passthrough done")
	if err := setupNamespacedVeth(ctx, PassthroughNames, params.TargetNS); err != nil {
		return fmt.Errorf("SetupPassthrough: failed to setup VNI veth: %w", err)
	}

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupVNI: Failed to get network namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	hostVeth, err := netlink.LinkByName(PassthroughNames.HostSide)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return fmt.Errorf("SetupPassthrough: host veth %s does not exist, cannot setup Passthrough", PassthroughNames.HostSide)
	}

	err = assignIPsToInterface(hostVeth, params.HostVeth.HostIPv4, params.HostVeth.HostIPv6)
	if err != nil {
		return fmt.Errorf("failed to assign IPs to host veth: %w", err)
	}

	if err := netnamespace.In(ns, func() error {
		peVeth, err := netlink.LinkByName(PassthroughNames.NamespaceSide)
		if err != nil {
			return fmt.Errorf("could not find peer veth %s in namespace %s: %w", PassthroughNames.NamespaceSide, params.TargetNS, err)
		}

		err = assignIPsToInterface(peVeth, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6)
		if err != nil {
			return fmt.Errorf("failed to assign IPs to PE veth: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// SetupGroutPassthrough moves a TAP interface created by grout from the router
// pod namespace to the host namespace and assigns IPs to it.
func SetupGroutPassthrough(ctx context.Context, tapName string, targetNS string, hostIPv4, hostIPv6 string) error {
	slog.InfoContext(ctx, "setup grout passthrough", "tapName", tapName, "targetNS", targetNS)
	defer slog.InfoContext(ctx, "setup grout passthrough done")

	// Check if the TAP already exists in the host namespace (idempotent).
	if link, err := netlink.LinkByName(tapName); err == nil {
		slog.InfoContext(ctx, "grout TAP already in host namespace", "tapName", tapName)
		if err := assignIPsToInterface(link, hostIPv4, hostIPv6); err != nil {
			return fmt.Errorf("failed to assign IPs to existing TAP %s: %w", tapName, err)
		}
		return nil
	}

	ns, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("failed to get network namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()

	// Move the TAP from the pod namespace to the host (current) namespace.
	if err := netnamespace.In(ns, func() error {
		tap, tapErr := netlink.LinkByName(tapName)
		if tapErr != nil {
			return fmt.Errorf("could not find TAP %s in namespace %s: %w", tapName, targetNS, tapErr)
		}

		// Get the host namespace fd. Since netnamespace.In saved the original
		// namespace before switching, we need to explicitly open it here.
		hostNS, hostNSErr := netns.GetFromPath("/proc/1/ns/net")
		if hostNSErr != nil {
			return fmt.Errorf("failed to get host network namespace: %w", hostNSErr)
		}
		defer func() {
			if closeErr := hostNS.Close(); closeErr != nil {
				slog.Error("failed to close host namespace handle", "error", closeErr)
			}
		}()

		if setErr := netlink.LinkSetNsFd(tap, int(hostNS)); setErr != nil {
			return fmt.Errorf("failed to move TAP %s to host namespace: %w", tapName, setErr)
		}
		slog.InfoContext(ctx, "moved grout TAP to host namespace", "tapName", tapName)
		return nil
	}); err != nil {
		return err
	}

	// Now in the host namespace: bring the TAP up and assign IPs.
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("could not find TAP %s in host namespace after move: %w", tapName, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		return fmt.Errorf("failed to set TAP %s up: %w", tapName, err)
	}
	if err := assignIPsToInterface(tap, hostIPv4, hostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to TAP %s: %w", tapName, err)
	}
	return nil
}

// RemoveGroutPassthrough removes the grout TAP interface from the host namespace.
func RemoveGroutPassthrough(tapName string) error {
	return removeLinkByName(tapName)
}

func RemovePassthrough(targetNS string) error {
	if err := removeLinkByName(PassthroughNames.HostSide); err != nil {
		return fmt.Errorf("RemovePassthrough: failed to remove host link %s: %w", PassthroughNames.HostSide, err)
	}

	ns, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("RemovePassthrough: failed to get network namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()

	if err := netnamespace.In(ns, func() error {
		if err := removeLinkByName(PassthroughNames.NamespaceSide); err != nil {
			return fmt.Errorf("remove namespace-side veth leg: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

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
