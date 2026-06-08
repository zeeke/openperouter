// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/openperouter/openperouter/internal/hostnetwork"
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

	portExists, err := client.portExists(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to check grout port %s: %w", portName, err)
	}

	if !portExists {
		_ = removeLinkByName(tapName)

		devargs := fmt.Sprintf("net_tap%d,iface=%s", params.VNI, tapName)
		if err := client.ensurePortInVRF(ctx, portName, devargs, params.VRF); err != nil {
			return fmt.Errorf("failed to create host session port: %w", err)
		}

		if err := moveLinkToHostNamespace(ctx, tapName, ns); err != nil {
			return fmt.Errorf("failed to move TAP to host namespace: %w", err)
		}

		if err := setUniqueMAC(tapName); err != nil {
			return fmt.Errorf("failed to set unique MAC on host TAP: %w", err)
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

	if err := assignIPsToGroutPort(ctx, client, portName, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	portAddresses, err := client.getAddresses(ctx, portName)
	if err != nil {
		return fmt.Errorf("failed to get grout %s port addresses: %w", portName, err)
	}

	for _, addr := range portAddresses {
		if err := ensureKernelSubnetRoute(ns, "main", addr); err != nil {
			return fmt.Errorf("failed to add kernel route for subnet %s: %w", addr, err)
		}
	}

	return nil
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
