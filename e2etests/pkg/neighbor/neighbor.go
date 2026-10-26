// SPDX-License-Identifier:Apache-2.0

package neighbor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
)

// IsKernelStale reports whether the kernel neighbor entry for ip on dev is
// STALE. It returns an error if the entry does not exist.
func IsKernelStale(exec executor.Executor, dev, ip string) (bool, error) {
	out, err := exec.Exec("ip", "-j", "neigh", "show", ip, "dev", dev)
	if err != nil {
		return false, fmt.Errorf("failed to show neighbor %s on %s: %w. Output: %s", ip, dev, err, out)
	}
	var entries []kernelNeighbor
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return false, fmt.Errorf("failed to parse neighbor output %q: %w", out, err)
	}
	if len(entries) == 0 {
		return false, fmt.Errorf("no neighbor entry for %s on %s", ip, dev)
	}
	return slices.Contains(entries[0].State, "STALE"), nil
}

// Monitor records the neighbor events of the given router's datapath until
// the returned function is called. The returned function stops the
// recording and returns what was collected.
func Monitor(router openperouter.RouterExecutor, grout bool) func() (string, error) {
	if grout {
		// grcli talks to grout over its socket, so it doesn't need to run
		// in the router's network namespace.
		exec := executor.ForPod(openperouter.Namespace, router.Name(), "frr")
		return monitor(exec, "grcli", "events", "show")
	}
	exec := executor.ForContainer(router.NodeName())
	return monitor(exec, "ip", "netns", "exec", openperouter.NamedNetns, "ip", "-ts", "monitor", "neigh")
}

type kernelNeighbor struct {
	State []string `json:"state"`
}

func monitor(exec executor.ExecutorWithContext, cmd string, args ...string) func() (string, error) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var output string
	var err error
	mu.Lock()
	go func() {
		defer mu.Unlock()
		output, err = exec.CommandContext(ctx, cmd, args...)
	}()

	return func() (string, error) {
		cancel()
		mu.Lock()
		defer mu.Unlock()
		return output, err
	}
}
