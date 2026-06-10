// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/conversion"
	"github.com/openperouter/openperouter/internal/grout"
	"github.com/openperouter/openperouter/internal/hostnetwork"
)

type groutConfiguration struct {
	targetNamespace string
	nodeIndex       int
	conversion.APIConfigData
	groutSocketPath string
}

func configureGroutDataPath(ctx context.Context, config groutConfiguration) error {
	groutClient := grout.NewClient(config.groutSocketPath)

	hasAlreadyUnderlay, err := grout.HasUnderlayInterface(ctx, groutClient)
	if err != nil {
		return fmt.Errorf("failed to check if target namespace %s has underlay: %w", config.targetNamespace, err)
	}
	if hasAlreadyUnderlay && len(config.Underlays) == 0 {
		cleanupGroutState(ctx, groutClient, config.targetNamespace)
		return nil
	}

	if len(config.Underlays) == 0 {
		return nil // nothing to do
	}

	// When grout has no state (fresh start or after crash), clean up any
	// stale kernel VRFs left behind by FRR's zebra. These VRFs persist in
	// the perouter named namespace across pod restarts and prevent grout
	// from creating and managing its own VRFs. This MUST happen before
	// the underlay is set up — deleting kernel VRFs while grout is active
	// crashes grout's dplane_frr module via netlink events.
	if !hasAlreadyUnderlay {
		vrfNames := make([]string, 0, len(config.L3VNIs))
		for _, l3vni := range config.L3VNIs {
			vrfNames = append(vrfNames, l3vni.Spec.VRF)
		}
		if err := grout.RemoveConflictingKernelVRFs(ctx, config.targetNamespace, vrfNames); err != nil {
			return fmt.Errorf("failed to remove conflicting kernel VRFs: %w", err)
		}
	}

	slog.InfoContext(ctx, "configure interface start", "namespace", config.targetNamespace)
	defer slog.InfoContext(ctx, "configure interface end", "namespace", config.targetNamespace)
	apiConfig := conversion.APIConfigData{
		Underlays:     config.Underlays,
		L3VNIs:        config.L3VNIs,
		L2VNIs:        config.L2VNIs,
		L3Passthrough: config.L3Passthrough,
	}
	hostConfig, err := conversion.APItoHostConfig(config.nodeIndex, config.targetNamespace, apiConfig)
	if err != nil {
		return fmt.Errorf("failed to convert config to host configuration: %w", err)
	}

	slog.InfoContext(ctx, "setting up underlay")
	if err := grout.SetupUnderlay(ctx, groutClient, hostConfig.Underlay); err != nil {
		return fmt.Errorf("failed to setup underlay: %w", err)
	}

	slog.InfoContext(ctx, "setting up passthrough")
	if hostConfig.L3Passthrough != nil {
		if err := grout.SetupPassthrough(ctx, groutClient, *hostConfig.L3Passthrough); err != nil {
			return fmt.Errorf("failed to setup passthrough: %w", err)
		}
	}

	if len(apiConfig.L3Passthrough) == 0 {
		if err := grout.RemovePassthrough(ctx, groutClient, config.targetNamespace); err != nil {
			return fmt.Errorf("failed to remove passthrough: %w", err)
		}
	}

	for _, l3vni := range hostConfig.L3VNIs {
		slog.InfoContext(ctx, "setting up L3VNI", "vrf", l3vni.VRF, "vni", l3vni.VNI)
		if err := grout.SetupL3VNI(ctx, groutClient, l3vni); err != nil {
			return fmt.Errorf("failed to setup L3VNI (VRF %s, VNI %d): %w", l3vni.VRF, l3vni.VNI, err)
		}
	}

	if err := grout.RemoveL3VNIs(ctx, groutClient, hostConfig.L3VNIs); err != nil {
		return fmt.Errorf("failed to remove stale L3VNIs: %w", err)
	}

	return nil
}

func cleanupGroutState(ctx context.Context, client *grout.Client, targetNamespace string) {
	slog.InfoContext(ctx, "underlay removed, cleaning up grout state")
	if err := grout.RemoveL3VNIs(ctx, client, nil); err != nil {
		slog.Warn("failed to remove L3VNIs after underlay removal", "err", err)
	}
	if err := grout.RemovePassthrough(ctx, client, targetNamespace); err != nil {
		slog.Warn("failed to remove passthrough after underlay removal", "err", err)
	}
	if err := hostnetwork.RemoveUnderlay(targetNamespace); err != nil {
		slog.Warn("failed to remove underlay interfaces after underlay removal", "err", err)
	}
}
