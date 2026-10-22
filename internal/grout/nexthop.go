// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// neighborLifetimeSeconds is how long grout keeps a learned neighbor
// reachable without a reply before probing it. It matches the kernel's
// default base_reachable_time: grout defaults to 20 minutes, which would let
// a neighbor that went away (e.g. a migrated pod) linger, along with its
// EVPN Type-2 route, for far longer than on the kernel datapath.
const neighborLifetimeSeconds = 30

type groutNexthopConfig struct {
	Lifetime int `json:"lifetime"`
}

// EnsureNeighborAgeing configures how grout ages its learned neighbors.
//
// Grout refreshes its neighbors on its own: once the lifetime expires it
// probes them, keeping the live ones reachable and failing the ones that
// don't answer. This is what the BridgeRefresher does on the kernel
// datapath, so on grout tuning the lifetime is all that is needed.
// The setting doesn't survive a grout restart, hence it is enforced on
// every reconciliation.
func EnsureNeighborAgeing(ctx context.Context, client *Client) error {
	if err := client.ensureNexthopLifetime(ctx, neighborLifetimeSeconds); err != nil {
		return fmt.Errorf("failed to set grout neighbor lifetime: %w", err)
	}
	return nil
}

func (c *Client) ensureNexthopLifetime(ctx context.Context, seconds int) error {
	out, err := c.runOutput(ctx, "nexthop", "config", "show")
	if err != nil {
		return fmt.Errorf("showing nexthop config: %w", err)
	}
	var config groutNexthopConfig
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		return fmt.Errorf("parsing nexthop config %q: %w", out, err)
	}
	if config.Lifetime == seconds {
		return nil
	}

	slog.InfoContext(ctx, "setting grout nexthop lifetime", "current", config.Lifetime, "desired", seconds)
	if err := c.run(ctx, "nexthop", "config", "set", "lifetime", strconv.Itoa(seconds)); err != nil {
		return fmt.Errorf("setting nexthop lifetime to %d: %w", seconds, err)
	}
	return nil
}
