// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

const (
	underlayPortName = "underlay"
)

func HasUnderlayInterface(ctx context.Context, client *Client) (bool, error) {
	return client.portExists(ctx, underlayPortName)
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
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
		// TODO: create a VRF grout port and assign the VTEP IP to it
	}

	return nil
}
