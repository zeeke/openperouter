// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const defaultVXLanPort int32 = 4789

func bridgeName(vni int32) string { return fmt.Sprintf("br-%d", vni) }
func vxlanName(vni int32) string  { return fmt.Sprintf("vx-%d", vni) }
func hsPortName(vni int32) string { return fmt.Sprintf("hs-%d", vni) }
func hsTapName(vni int32) string  { return fmt.Sprintf("tap_hs_%d", vni) }

func SetupL3VNI(ctx context.Context, client *Client, params hostnetwork.L3VNIParams) error {
	slog.DebugContext(ctx, "setup L3VNI", "vrf", params.VRF, "vni", params.VNI)
	defer slog.DebugContext(ctx, "setup L3VNI done", "vrf", params.VRF, "vni", params.VNI)

	vtepIP, err := vtepIPFromCIDR(params.VTEPIP)
	if err != nil {
		return fmt.Errorf("SetupL3VNI: %w", err)
	}

	vxlanPort := defaultVXLanPort
	if params.VXLanPort != nil {
		vxlanPort = *params.VXLanPort
	}

	vrfExistsInGrout, err := client.portExists(ctx, params.VRF)
	if err != nil {
		return fmt.Errorf("SetupL3VNI: checking VRF %s: %w", params.VRF, err)
	}
	if !vrfExistsInGrout {
		// FRR's zebra auto-creates kernel VRFs from its cached config.
		// These can reappear after RemoveConflictingKernelVRFs ran,
		// blocking grout VRF creation with EEXIST.
		if err := RemoveConflictingKernelVRFs(ctx, params.TargetNS, []string{params.VRF}); err != nil {
			return fmt.Errorf("SetupL3VNI: failed to remove conflicting kernel VRF %s: %w", params.VRF, err)
		}
	}

	if err := client.ensureVRF(ctx, params.VRF); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to create VRF %s: %w", params.VRF, err)
	}

	bridge := bridgeName(params.VNI)
	if err := client.ensureBridge(ctx, bridge, params.VRF); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to create bridge %s: %w", bridge, err)
	}

	vxlan := vxlanName(params.VNI)
	if err := client.ensureVXLAN(ctx, vxlan, params.VNI, vtepIP, bridge, vxlanPort); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to create VXLAN %s: %w", vxlan, err)
	}

	if params.HostVeth == nil {
		return nil
	}

	if err := setupL3VNIHostSession(ctx, client, params); err != nil {
		return fmt.Errorf("SetupL3VNI: %w", err)
	}

	return nil
}

func setupL3VNIHostSession(ctx context.Context, client *Client, params hostnetwork.L3VNIParams) error {
	portName := hsPortName(params.VNI)
	tapName := hsTapName(params.VNI)

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("failed to find namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	// Remove old kernel interfaces from the non-grout L3VNI path.
	// The veths have the same IPs as the grout TAP and loopback,
	// causing routing conflicts that prevent BGP sessions.
	oldHostVeth := fmt.Sprintf("host-%d", params.VNI)
	if err := removeLinkByName(oldHostVeth); err != nil {
		slog.WarnContext(ctx, "failed to remove old host veth", "name", oldHostVeth, "error", err)
	}
	_ = netnamespace.In(ns, func() error {
		for _, name := range []string{
			fmt.Sprintf("pe-%d", params.VNI),
			fmt.Sprintf("vni%d", params.VNI),
			fmt.Sprintf("br-pe-%d", params.VNI),
		} {
			if err := removeLinkByName(name); err != nil {
				slog.WarnContext(ctx, "failed to remove old kernel interface", "name", name, "error", err)
			}
		}
		return nil
	})

	portExists, err := client.portExists(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to check grout port %s: %w", portName, err)
	}

	portExists, err = recoverStalePort(ctx, client, ns, portName, tapName, portExists)
	if err != nil {
		return err
	}

	if !portExists {
		if err := createL3VNITap(ctx, client, ns, params, portName, tapName); err != nil {
			return err
		}
	} else {
		slog.InfoContext(ctx, "grout L3VNI host session port already exists, skipping TAP setup", "port", portName)
	}

	hostTap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("host TAP %s not found after move: %w", tapName, err)
	}
	if err := assignIPsToLink(hostTap, params.HostVeth.HostIPv4, params.HostVeth.HostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to host TAP: %w", err)
	}

	return nil
}

// recoverStalePort checks whether a grout port exists but its TAP device is
// missing from the host namespace (partial setup from a previous run).
// If so, it deletes the stale port so it can be fully recreated.
func recoverStalePort(ctx context.Context, client *Client, ns netns.NsHandle, portName, tapName string, portExists bool) (bool, error) {
	if !portExists {
		return false, nil
	}
	if _, tapErr := netlink.LinkByName(tapName); tapErr == nil {
		return true, nil
	}
	slog.InfoContext(ctx, "grout port exists but TAP not in host NS, recreating",
		"port", portName, "tap", tapName)
	if err := client.deletePort(ctx, portName); err != nil {
		return false, fmt.Errorf("failed to delete stale grout port %s: %w", portName, err)
	}
	_ = netnamespace.In(ns, func() error {
		return removeLinkByName(tapName)
	})
	return false, nil
}

func createL3VNITap(ctx context.Context, client *Client, ns netns.NsHandle, params hostnetwork.L3VNIParams, portName, tapName string) error {
	_ = removeLinkByName(tapName)

	devargs := fmt.Sprintf("net_tap%d,iface=%s", params.VNI, tapName)
	if err := client.ensurePortInVRF(ctx, portName, devargs, params.VRF); err != nil {
		return fmt.Errorf("failed to create host session port: %w", err)
	}

	if err := assignIPsWithRetry(ctx, client, portName, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	// Grout creates a kernel VRF device and a TUN (gr-loop) when the
	// first port joins a custom VRF. Assign the PE-side IPs to the
	// TUN so FRR's kernel TCP stack can bind to them in this VRF.
	if err := assignIPsToVRFLoopback(ctx, ns, params.VRF,
		params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to VRF loopback: %w", err)
	}

	if err := moveLinkToHostNamespace(ctx, tapName, ns); err != nil {
		return fmt.Errorf("failed to move TAP to host namespace: %w", err)
	}

	return setUniqueMAC(tapName)
}

func RemoveL3VNIs(ctx context.Context, client *Client, configured []hostnetwork.L3VNIParams) error {
	slog.DebugContext(ctx, "removing stale L3VNIs")
	defer slog.DebugContext(ctx, "removing stale L3VNIs done")

	configuredVNIs := make(map[int32]bool)
	for _, p := range configured {
		configuredVNIs[p.VNI] = true
	}

	out, err := client.runOutput(ctx, "interface", "show")
	if err != nil {
		return fmt.Errorf("RemoveL3VNIs: failed to list interfaces: %w", err)
	}

	staleVNIs := findStaleVNIs(out, configuredVNIs)
	for _, vni := range staleVNIs {
		slog.InfoContext(ctx, "removing stale L3VNI", "vni", vni)

		if err := client.deleteInterface(ctx, vxlanName(vni)); err != nil {
			return fmt.Errorf("RemoveL3VNIs: failed to delete VXLAN vx-%d: %w", vni, err)
		}
		if err := client.deleteInterface(ctx, bridgeName(vni)); err != nil {
			return fmt.Errorf("RemoveL3VNIs: failed to delete bridge br-%d: %w", vni, err)
		}
		if err := client.deleteInterface(ctx, hsPortName(vni)); err != nil {
			return fmt.Errorf("RemoveL3VNIs: failed to delete host session port hs-%d: %w", vni, err)
		}
		_ = removeLinkByName(hsTapName(vni))
	}

	return nil
}

func vtepIPFromCIDR(cidr string) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		if parsed := net.ParseIP(cidr); parsed != nil {
			return parsed.String(), nil
		}
		return "", fmt.Errorf("failed to parse VTEP IP %q: %w", cidr, err)
	}
	return ip.String(), nil
}

func findStaleVNIs(interfaceShowOutput string, configuredVNIs map[int32]bool) []int32 {
	var stale []int32
	for _, line := range strings.Split(interfaceShowOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var vni int32
		if n, _ := fmt.Sscanf(fields[0], "vx-%d", &vni); n == 1 {
			if !configuredVNIs[vni] {
				stale = append(stale, vni)
			}
		}
	}
	return stale
}
