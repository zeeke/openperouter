// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/conversion"
	"github.com/openperouter/openperouter/internal/grout"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/hostnetwork/bridgerefresh"
	"github.com/openperouter/openperouter/internal/sysctl"
)

type interfacesConfiguration struct {
	targetNamespace    string
	underlayFromMultus bool
	groutEnabled       bool
	groutSocketPath    string
	nodeIndex          int
	conversion.ApiConfigData
}

type UnderlayRemovedError struct{}

func (n UnderlayRemovedError) Error() string {
	return "no underlays configured"
}

func configureInterfaces(ctx context.Context, config interfacesConfiguration) error {
	hasAlreadyUnderlay, err := hostnetwork.HasUnderlayInterface(config.targetNamespace)
	if err != nil {
		return fmt.Errorf("failed to check if target namespace %s has underlay: %w", config.targetNamespace, err)
	}
	if hasAlreadyUnderlay && len(config.Underlays) == 0 {
		return UnderlayRemovedError{}
	}

	if len(config.Underlays) == 0 {
		return nil // nothing to do
	}

	slog.InfoContext(ctx, "configure interface start", "namespace", config.targetNamespace)
	defer slog.InfoContext(ctx, "configure interface end", "namespace", config.targetNamespace)
	apiConfig := conversion.ApiConfigData{
		Underlays:     config.Underlays,
		L3VNIs:        config.L3VNIs,
		L2VNIs:        config.L2VNIs,
		L3Passthrough: config.L3Passthrough,
	}
	hostConfig, err := conversion.APItoHostConfig(config.nodeIndex, config.targetNamespace, config.underlayFromMultus, apiConfig)
	if err != nil {
		return fmt.Errorf("failed to convert config to host configuration: %w", err)
	}

	slog.InfoContext(ctx, "ensuring sysctls", "groutEnabled", config.groutEnabled)
	sysctls := []sysctl.Sysctl{
		sysctl.ArpAcceptAll(),
		sysctl.ArpAcceptDefault(),
		sysctl.AcceptUntrackedNADefault(),
		sysctl.AcceptUntrackedNAAll(),
	}
	if config.groutEnabled {
		// When grout is the dataplane, disable kernel forwarding so grout handles it.
		sysctls = append(sysctls, sysctl.DisableIPv4Forwarding(), sysctl.DisableIPv6Forwarding())
	} else {
		sysctls = append(sysctls, sysctl.IPv4Forwarding(), sysctl.IPv6Forwarding())
	}
	if err := sysctl.Ensure(config.targetNamespace, sysctls...); err != nil {
		return fmt.Errorf("failed to ensure sysctls: %w", err)
	}

	slog.InfoContext(ctx, "setting up underlay")
	if err := hostnetwork.SetupUnderlay(ctx, hostConfig.Underlay); err != nil {
		return fmt.Errorf("failed to setup underlay: %w", err)
	}
	for _, vni := range hostConfig.L3VNIs {
		slog.InfoContext(ctx, "setting up VNI", "vni", vni.VRF)
		if err := hostnetwork.SetupL3VNI(ctx, vni); err != nil {
			return fmt.Errorf("failed to setup vni: %w", err)
		}
	}

	for _, vni := range hostConfig.L2VNIs {
		slog.InfoContext(ctx, "setting up L2VNI", "vni", vni.VNI)
		if err := hostnetwork.SetupL2VNI(ctx, vni); err != nil {
			return fmt.Errorf("failed to setup vni: %w", err)
		}
		if err := bridgerefresh.StartForVNI(ctx, vni); err != nil {
			return fmt.Errorf("failed to start bridge refresher for vni %d: %w", vni.VNI, err)
		}
	}

	slog.InfoContext(ctx, "setting up passthrough")
	if hostConfig.L3Passthrough != nil {
		if err := hostnetwork.SetupPassthrough(ctx, *hostConfig.L3Passthrough); err != nil {
			return fmt.Errorf("failed to setup passthrough: %w", err)
		}
	}

	// When grout is enabled, create corresponding grout port interfaces for
	// the underlay NIC and passthrough veth so grout handles forwarding.
	if config.groutEnabled {
		if err := configureGroutInterfaces(ctx, config, hostConfig); err != nil {
			return fmt.Errorf("failed to configure grout interfaces: %w", err)
		}
	}

	slog.InfoContext(ctx, "removing deleted vnis")
	toCheck := make([]hostnetwork.VNIParams, 0, len(hostConfig.L3VNIs)+len(hostConfig.L2VNIs))
	for _, vni := range hostConfig.L3VNIs {
		toCheck = append(toCheck, vni.VNIParams)
	}
	for _, l2vni := range hostConfig.L2VNIs {
		toCheck = append(toCheck, l2vni.VNIParams)
	}
	if err := hostnetwork.RemoveNonConfiguredVNIs(config.targetNamespace, toCheck); err != nil {
		return fmt.Errorf("failed to remove deleted vnis: %w", err)
	}
	bridgerefresh.StopForRemovedVNIs(hostConfig.L2VNIs)

	if len(apiConfig.L3Passthrough) == 0 {
		if config.groutEnabled {
			client := grout.NewClient(config.groutSocketPath)
			ptName := hostnetwork.PassthroughNames.NamespaceSide
			if err := client.DeletePort(ctx, "pt-"+ptName); err != nil {
				return fmt.Errorf("failed to delete grout passthrough port: %w", err)
			}
		}
		if err := hostnetwork.RemovePassthrough(config.targetNamespace); err != nil {
			return fmt.Errorf("failed to remove passthrough: %w", err)
		}
	}
	return nil
}

// configureGroutInterfaces creates grout port interfaces that mirror the
// kernel interfaces, allowing grout to handle packet forwarding via DPDK.
func configureGroutInterfaces(ctx context.Context, config interfacesConfiguration, hostConfig conversion.HostConfigData) error {
	slog.InfoContext(ctx, "configuring grout interfaces")
	client := grout.NewClient(config.groutSocketPath)

	// For each Underlay NIC, create a grout port with net_tap devargs
	// that mirrors traffic from the kernel interface.
	if hostConfig.Underlay.UnderlayInterface != "" {
		nicName := hostConfig.Underlay.UnderlayInterface
		if err := client.EnsurePort(ctx, "ul-"+nicName, grout.UnderlayDevargs(nicName)); err != nil {
			return fmt.Errorf("failed to create grout port for underlay NIC %s: %w", nicName, err)
		}
	}

	// For the passthrough veth pair, create a grout port linked to the
	// namespace-side veth so grout can forward traffic through it.
	if hostConfig.L3Passthrough != nil {
		ptName := hostnetwork.PassthroughNames.NamespaceSide
		if err := client.EnsurePort(ctx, "pt-"+ptName, grout.PassthroughDevargs(ptName)); err != nil {
			return fmt.Errorf("failed to create grout port for passthrough veth %s: %w", ptName, err)
		}
	}

	return nil
}

// nonRecoverableHostError tells whether the router pod
// should be restarted instead of being reconfigured.
func nonRecoverableHostError(e error) bool {
	if errors.As(e, &UnderlayRemovedError{}) {
		return true
	}
	underlayExistsError := hostnetwork.UnderlayExistsError("")
	return errors.As(e, &underlayExistsError)
}
