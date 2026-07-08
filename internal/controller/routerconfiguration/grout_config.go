// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/conversion"
	openpeerrors "github.com/openperouter/openperouter/internal/errors"
	"github.com/openperouter/openperouter/internal/grout"
	"github.com/openperouter/openperouter/internal/hostnetwork"
)

// GroutDatapathConfigurator configures the host via the grout DPDK daemon.
type GroutDatapathConfigurator struct {
	conversion.GroutDatapathConfigValidator

	groutSocketPath string
}

func NewGroutConfigurator(groutSocketPath string) *GroutDatapathConfigurator {
	return &GroutDatapathConfigurator{
		groutSocketPath: groutSocketPath,
	}
}

func (g *GroutDatapathConfigurator) Configure(ctx context.Context, config interfacesConfiguration) error {
	groutClient := grout.NewClient(g.groutSocketPath)

	hasAlreadyUnderlay, err := grout.HasUnderlayInterface(ctx, groutClient)
	if err != nil {
		return fmt.Errorf("failed to check if target namespace %s has underlay: %w", config.targetNamespace, err)
	}
	if hasAlreadyUnderlay && len(config.Underlays) == 0 {
		slog.InfoContext(ctx, "underlay removed, cleaning up grout underlay")
		if err := grout.RestoreUnderlay(ctx, groutClient, config.targetNamespace); err != nil {
			slog.Warn("failed to remove underlay after underlay removal", "err", err)
		}
		return nil
	}

	if len(config.Underlays) == 0 {
		return nil // nothing to do
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

	var resourceErrors []error
	failedL3Domains := sets.New[string]()
	reason := v1alpha1.FailedResourceReasonOverlayAttachmentFailed

	var configuredL3VNIs []hostnetwork.L3VNIParams
	for _, l3vni := range hostConfig.L3VNIs {
		slog.InfoContext(ctx, "setting up L3VNI", "vrf", l3vni.VRF, "vni", l3vni.VNI)
		if err := grout.SetupL3VNI(ctx, groutClient, l3vni); err != nil {
			resourceErrors = append(resourceErrors, &openpeerrors.ResourceError{
				Obj: v1alpha1.FailedResource{
					Kind: openpeerrors.KindL3VNI, Name: l3vni.Name, Reason: reason, Message: err.Error(),
				},
			})
			failedL3Domains.Insert(l3vni.VRF)
			continue
		}
		configuredL3VNIs = append(configuredL3VNIs, l3vni)
	}

	var configuredL2VNIs []hostnetwork.L2VNIParams
	for _, l2vni := range hostConfig.L2VNIs {
		if failedL3Domains.Has(l2vni.VRF) {
			resourceErrors = append(resourceErrors, &openpeerrors.ResourceError{
				Obj: v1alpha1.FailedResource{
					Kind: openpeerrors.KindL2VNI, Name: l2vni.Name, Reason: reason,
					Message: fmt.Sprintf("L3 domain %q failed grout provisioning", l2vni.VRF),
				},
			})
			continue
		}
		slog.InfoContext(ctx, "setting up L2VNI", "vni", l2vni.VNI)
		if err := grout.SetupL2VNI(ctx, groutClient, l2vni); err != nil {
			resourceErrors = append(resourceErrors, &openpeerrors.ResourceError{
				Obj: v1alpha1.FailedResource{
					Kind: openpeerrors.KindL2VNI, Name: l2vni.Name, Reason: reason, Message: err.Error(),
				},
			})
			continue
		}
		configuredL2VNIs = append(configuredL2VNIs, l2vni)
	}

	slog.InfoContext(ctx, "setting up passthrough")
	if hostConfig.L3Passthrough != nil {
		if err := grout.SetupPassthrough(ctx, groutClient, *hostConfig.L3Passthrough); err != nil {
			resourceErrors = append(resourceErrors, &openpeerrors.ResourceError{
				Obj: v1alpha1.FailedResource{
					Kind: openpeerrors.KindL3Passthrough, Name: config.L3Passthrough[0].Name, Reason: reason, Message: err.Error(),
				},
			})
		}
	}

	if len(apiConfig.L3Passthrough) == 0 {
		if err := grout.RemovePassthrough(ctx, groutClient); err != nil {
			return fmt.Errorf("failed to remove passthrough: %w", err)
		}
	}

	configuredVNIs := make([]hostnetwork.VNIParams, 0, len(configuredL3VNIs)+len(configuredL2VNIs))
	for _, vni := range configuredL3VNIs {
		configuredVNIs = append(configuredVNIs, vni.VNIParams)
	}
	for _, vni := range configuredL2VNIs {
		configuredVNIs = append(configuredVNIs, vni.VNIParams)
	}
	if err := grout.RemoveNonConfiguredVNIs(ctx, groutClient, configuredVNIs); err != nil {
		return fmt.Errorf("failed to remove stale VNIs: %w", err)
	}

	return errors.Join(resourceErrors...)
}
