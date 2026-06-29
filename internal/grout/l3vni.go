// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/utils/ptr"
)

func SetupL3VNI(ctx context.Context, client *Client, params hostnetwork.L3VNIParams) error {
	slog.DebugContext(ctx, "setup L3VNI", "vrf", params.VRF, "vni", params.VNI)
	defer slog.DebugContext(ctx, "setup L3VNI done", "vrf", params.VRF, "vni", params.VNI)

	if err := setupVNI(ctx, client, params.VNIParams); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to setup VNI: %w", err)
	}

	if params.LinkIPs == nil {
		slog.DebugContext(ctx, "no host TAP configured, skipping setup")
		return nil
	}

	linkPair := linkPairNamesFromVNI(params.VNI)

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupPassthrough: failed to find namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	if err := ensureTapPortInHostNamespace(ctx, client, linkPair.NamespaceSide, linkPair.HostSide, params.VRF, ns); err != nil {
		return err
	}

	hostTap, err := netlink.LinkByName(linkPair.HostSide)
	if err != nil {
		return fmt.Errorf("host TAP %s not found after move: %w", linkPair.HostSide, err)
	}
	if err := hostnetwork.AssignIPsToInterface(hostTap, params.LinkIPs.HostIPv4, params.LinkIPs.HostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to host TAP: %w", err)
	}

	underlayMTU, err := hostnetwork.FindUnderlayMTU(ns)
	if err != nil {
		return fmt.Errorf("could not find underlay MTU: %w", err)
	}
	if err := hostnetwork.SetVethMTUForTunnelOverhead(hostTap, underlayMTU, hostnetwork.VXLanOverhead); err != nil {
		return fmt.Errorf("failed to set MTU on host TAP %s: %w", linkPair.HostSide, err)
	}

	if err := ensurePortAddresses(ctx, client, linkPair.NamespaceSide, params.LinkIPs.NSIPv4, params.LinkIPs.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	return nil
}

func setupVNI(ctx context.Context, client *Client, params hostnetwork.VNIParams) error {
	if err := client.ensureVRF(ctx, params.VRF); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to create VRF %s: %w", params.VRF, err)
	}

	vtepIP, _, err := net.ParseCIDR(params.VTEPIP)
	if err != nil {
		return fmt.Errorf("failed to parse vtep ip %v: %w", params.VTEPIP, err)
	}

	if err := client.ensureVXLAN(ctx,
		fmt.Sprintf("vni%d", params.VNI),
		vtepIP.String(),
		params.VRF,
		params.VNI,
		ptr.Deref(params.VXLanPort, 4789),
	); err != nil {
		return fmt.Errorf("failed to create vxlan interface: %w", err)
	}

	return nil
}

func RemoveAllVNIs(ctx context.Context, client *Client) error {
	return RemoveNonConfiguredVNIs(ctx, client, []hostnetwork.VNIParams{})
}

func RemoveNonConfiguredVNIs(ctx context.Context, client *Client, configured []hostnetwork.VNIParams) error {
	slog.DebugContext(ctx, "removing stale VNIs")
	defer slog.DebugContext(ctx, "removing stale VNIs done")

	configuredVNIs := make(map[int32]bool)
	configuredVRFs := make(map[string]bool)

	for _, p := range configured {
		configuredVNIs[p.VNI] = true
		configuredVRFs[p.VRF] = true
	}

	ifaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("RemoveNonConfiguredVNIs: failed to list interfaces: %w", err)
	}

	staleVNIs := findStaleVNIs(ifaces, configuredVNIs)
	for _, vni := range staleVNIs {
		slog.InfoContext(ctx, "removing stale VNI", "vni", vni)
		if err := removeVNI(ctx, client, vni); err != nil {
			return fmt.Errorf("RemoveNonConfiguredVNIs: failed to remove VNI %d: %w", vni, err)
		}
	}

	staleVRFs := findStaleVRFs(ifaces, configuredVRFs)
	for _, vrf := range staleVRFs {
		if vrf == "main" {
			continue
		}
		slog.InfoContext(ctx, "removing stale VRF", "vrf", vrf)
		if err := client.deleteInterface(ctx, vrf); err != nil {
			return fmt.Errorf("failed to delete VRF %s: %w", vrf, err)
		}
	}

	failedDeletes := hostnetwork.RemoveHostSideVNIs(configuredVNIs)
	return errors.Join(failedDeletes...)
}

func removeVNI(ctx context.Context, client *Client, vni int32) error {
	linkPair := linkPairNamesFromVNI(vni)

	if err := client.deletePort(ctx, linkPair.NamespaceSide); err != nil {
		return fmt.Errorf("failed to delete grout port %s: %w", linkPair.NamespaceSide, err)
	}

	_ = hostnetwork.RemoveLinkByName(linkPair.HostSide)

	bridgeName := hostnetwork.BridgeName(vni)
	if err := client.deleteInterface(ctx, bridgeName); err != nil {
		slog.DebugContext(ctx, "bridge delete (may not exist)", "bridge", bridgeName, "err", err)
	}

	vxlanName := fmt.Sprintf("vni%d", vni)
	if err := client.deleteInterface(ctx, vxlanName); err != nil {
		return fmt.Errorf("failed to delete VXLAN %s: %w", vxlanName, err)
	}

	return nil
}

func findStaleVNIs(ifaces []groutInterface, configuredVNIs map[int32]bool) []int32 {
	var stale []int32
	for _, iface := range ifaces {
		var vni int32
		if n, _ := fmt.Sscanf(iface.Name, "vni%d", &vni); n == 1 {
			if !configuredVNIs[vni] {
				stale = append(stale, vni)
			}
		}
	}
	return stale
}

func findStaleVRFs(ifaces []groutInterface, configuredVRFs map[string]bool) []string {
	var stale []string
	for _, iface := range ifaces {
		if iface.Type != "vrf" {
			continue
		}

		if !configuredVRFs[iface.Name] {
			stale = append(stale, iface.Name)
		}

	}
	return stale
}

const hostTapPrefix = "host-"
const pePortPrefix = "pe-"

// linkPairNamesFromVNI returns the names of the link pair legs
// corresponding to the default namespace and the target namespace, based on VNI.
func linkPairNamesFromVNI(vni int32) hostnetwork.VethNames {
	hostSide := fmt.Sprintf("%s%d", hostTapPrefix, vni)
	peSide := fmt.Sprintf("%s%d", pePortPrefix, vni)
	return hostnetwork.VethNames{HostSide: hostSide, NamespaceSide: peSide}
}
