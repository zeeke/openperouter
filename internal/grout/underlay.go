// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

// HasUnderlayInterface checks whether the given namespace has an underlay interface configured.
func HasUnderlayInterface(namespace string) (bool, error) {
	// TODO: implement grout-based underlay check
	return false, nil
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
func SetupUnderlay(ctx context.Context, params hostnetwork.UnderlayParams) error {
	// TODO: implement grout-based underlay setup
	return nil
}
