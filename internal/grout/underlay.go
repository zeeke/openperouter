// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

const underlayPortName = "underlay"
const underlayLoopback = "lound"

// HasUnderlayInterface checks whether the given namespace has an underlay interface configured.
func HasUnderlayInterface(namespace string) (bool, error) {
	// TODO: implement grout-based underlay check
	return false, nil
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
// It creates a grout TAP port with the `remote` devarg to connect to the underlay NIC,
// and sets up a loopback interface in the target namespace for the VTEP IP if EVPN is configured.
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams) error {
	slog.DebugContext(ctx, "setup underlay", "params", params)
	defer slog.DebugContext(ctx, "setup underlay done")

	if params.UnderlayInterface == "" {
		return nil
	}

	// Create a grout TAP port connected to the underlay NIC via the `remote` devarg.
	// This sets up tc rules to forward traffic between the TAP and the kernel NIC.
	devargs := fmt.Sprintf("net_tap0,remote=%s", params.UnderlayInterface)
	if err := client.ensurePort(ctx, underlayPortName, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	if params.EVPN != nil && params.EVPN.VtepIP != "" {
		// TODO: create a VRF grout port and assign the VTEP IP to it, instead of using a loopback. This will allow better isolation and more flexible routing.
	}

	return nil
}
