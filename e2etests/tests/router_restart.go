// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	"github.com/openperouter/openperouter/e2etests/pkg/systemd"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// RestartBGPD restarts bgpd on every router.
//
// It is wired as Updater.OnUnderlayReset so that it runs whenever the suite
// tears all resources down and whenever it creates an underlay on a cluster
// that runs none. Removing an underlay while the fabric still advertises
// VPN routes makes bgpd keep the deleted default instance around as
// "hidden" with its EVPN data freed, and BGP VRF instances whose VRF went
// away linger with equally broken state. Recreating the underlay resurrects
// that instance without reallocating the EVPN data, and the next
// "show running-config" - which the reloader runs on every config update -
// dereferences the NULL pointer and crashes bgpd. watchfrr restarts it in
// the middle of the config churn, leaving the datapath (grout in particular)
// in a state subsequent specs time out on. Resetting on teardown kills the
// leftovers before the removal churn starts; resetting before the new
// underlay is applied gives the resurrected path no instance to bring back.
//
// This is a workaround for an upstream FRR bug, reported in
// FRRouting/frr#23835 with fixes pending in FRRouting/frr#23851 and
// FRRouting/frr#24569 (only one needs to merge); FRRouting/frr#24213 is a
// related crash on the same path, already fixed on master and stable/10.8.
// Since 10.7.0, "no router bgp" followed by "router bgp" with the same ASN,
// while VPN routes are present, brings back a hidden instance whose EVPN
// state was freed, and the next "show running-config" segfaults bgpd. The
// restart can be dropped once the routers run an FRR release that includes
// these fixes.
//
// Restarting bgpd before the new underlay is applied gives it a clean
// instance, so the crash never triggers. The router pods are left in place:
// specs hold references to them (executors, pod names) that a pod restart
// would invalidate. On systemd deployments the router unit is restarted
// instead, because the kind nodes load the FRR AppArmor profiles, which
// deny signaling bgpd from inside the container.
func RestartBGPD() error {
	cs := k8sclient.New()
	if HostMode {
		return restartRouterUnits(cs)
	}
	routers, err := openperouter.Get(cs, false)
	if err != nil {
		return fmt.Errorf("failed to get the routers: %w", err)
	}
	for router := range routers.GetExecutors() {
		if err := restartBGPD(router); err != nil {
			return fmt.Errorf("failed to restart bgpd on router %s: %w", router.Name(), err)
		}
	}
	return nil
}

// restartRouterUnits restarts the router pod unit on every node. The unit
// stop is performed by the container runtime, which the AppArmor profiles
// do not restrict, and the new container starts a clean bgpd. The named
// network namespace survives the restart, and so do the node-based
// executors the specs hold.
func restartRouterUnits(cs kubernetes.Interface) error {
	nodes, err := k8s.GetNodes(cs)
	if err != nil {
		return fmt.Errorf("failed to get the nodes: %w", err)
	}
	for _, node := range nodes {
		nodeExec := executor.ForNode(node.Name)
		if err := systemd.RestartSystemdUnit(nodeExec, routerPodUnit); err != nil {
			return fmt.Errorf("failed to restart %s on node %s: %w", routerPodUnit, node.Name, err)
		}
	}

	routers, err := openperouter.Get(cs, true)
	if err != nil {
		return fmt.Errorf("failed to get the routers: %w", err)
	}
	for router := range routers.GetExecutors() {
		if err := waitBGPD(router); err != nil {
			return fmt.Errorf("bgpd did not come back on router %s: %w", router.Name(), err)
		}
	}
	return nil
}

func restartBGPD(router openperouter.RouterExecutor) error {
	// The listing snapshots /proc: a process exiting while awk walks it
	// makes awk fail on the vanished status file, so retry - the next
	// listing no longer contains it.
	var pids map[string][]string
	listingBackoff := wait.Backoff{Steps: 5, Duration: time.Second}
	err := retry.OnError(listingBackoff, func(error) bool { return true }, func() error {
		var err error
		pids, err = pidsForProcesses(router, []string{"bgpd", "watchfrr"})
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to find the bgpd process: %w", err)
	}
	for _, pid := range pids["bgpd"] {
		out, err := router.Exec("kill", "-9", pid)
		// The process list is a snapshot: bgpd may exit between reading
		// it and the kill (for example while watchfrr is restarting it),
		// which the shell reports as "No such process". The daemon being
		// gone is what this function wants, so only other failures are
		// fatal.
		if err != nil && !strings.Contains(out, "No such process") {
			return fmt.Errorf("failed to kill bgpd pid %s: %w (output: %q)", pid, err, out)
		}
	}

	// Start bgpd right away instead of leaving the restart to watchfrr:
	// its restart path is rate limited with a delay that doubles on every
	// restart (60s to 600s), so relying on it would make each successive
	// cleanup slower. Restarting within one watchfrr poll period also
	// keeps the supervisor from forking its own restart, so the rate
	// limit never kicks in.
	//
	// The start command must not inherit the exec session's output pipe:
	// FRR redirects a daemonizing process' stdio to /dev/null only when
	// it is a tty, and the generated configuration logs to stdout, so
	// bgpd would hold the pipe open and the exec would never return (the
	// lane then runs into the suite timeout). Point it at watchfrr's own
	// stdout instead, which is the container log, so the daemon keeps
	// logging where the triage dumps collect it.
	out, err := router.Exec("sh", "-c", startBGPDCommand(pids["watchfrr"]))
	if err != nil {
		// A failed start command is not fatal by itself: watchfrr also
		// restarts the daemon it saw dying, within its poll period. Let
		// the readiness wait decide, and report both errors only when
		// bgpd really does not come back.
		if waitErr := waitBGPD(router); waitErr != nil {
			return fmt.Errorf("bgpd did not come back (start: %v, output: %q; wait: %w)",
				err, out, waitErr)
		}
		return dropBGPInstances(router)
	}
	if err := waitBGPD(router); err != nil {
		return fmt.Errorf("bgpd did not come back: %w", err)
	}
	return dropBGPInstances(router)
}

// startBGPDCommand builds the shell command that restarts bgpd with its
// output detached from the exec session. Without a known watchfrr pid the
// output is discarded.
func startBGPDCommand(watchfrrPids []string) string {
	stdout := "/dev/null"
	if len(watchfrrPids) > 0 {
		stdout = fmt.Sprintf("/proc/%s/fd/1", watchfrrPids[0])
	}
	return fmt.Sprintf("/usr/lib/frr/watchfrr.sh start bgpd >%s 2>&1 </dev/null", stdout)
}

// dropBGPInstances removes every BGP instance from the running config.
//
// The started daemon loads the config file it was deployed with, which in
// split deployments is stale: it recreates a previous underlay's instances,
// the fabric sessions come back up and feed VPN routes in, and the config
// removal that follows would then hit populated VPN tables - the exact
// condition that leaves the broken hidden instances behind. Dropping the
// instances right after the start, before any session can exchange routes,
// leaves a clean daemon that the config application rebuilds from scratch.
//
// The controller's config application runs concurrently and may remove or
// add instances between the listing and the removals, so the whole wipe
// retries: each pass re-lists and removes whatever is left.
func dropBGPInstances(router openperouter.RouterExecutor) error {
	backoff := wait.Backoff{Steps: 5, Duration: time.Second}
	alwaysRetry := func(error) bool { return true }
	return retry.OnError(backoff, alwaysRetry, func() error {
		return dropBGPInstancesOnce(router)
	})
}

func dropBGPInstancesOnce(router openperouter.RouterExecutor) error {
	out, err := router.Exec("vtysh", "-c", "show running-config")
	if err != nil {
		return fmt.Errorf("failed to read the running config: %w (output: %q)", err, out)
	}
	instances := map[string][]string{} // instance -> l3vni numbers bound to it
	var current string
	for line := range strings.Lines(out) {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "router bgp"):
			current = trimmed
			instances[current] = nil
		case current != "" && strings.HasPrefix(trimmed, "vni "):
			instances[current] = append(instances[current], trimmed)
		}
	}
	// VRF instances depend on their default one, and the running config
	// lists the default first, so remove them in reverse order.
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		// FRR refuses to remove a VRF instance while an L3VNI is bound
		// to it, so unbind those first.
		for _, vni := range instances[name] {
			out, err := router.Exec("vtysh", "-c", "configure terminal", "-c", name,
				"-c", "no "+vni)
			if err != nil && !strings.Contains(out, "Can't find BGP instance") {
				return fmt.Errorf("failed to remove %q from %q: %w (output: %q)",
					vni, name, err, out)
			}
		}
		out, err := router.Exec("vtysh", "-c", "configure terminal", "-c", "no "+name)
		if err != nil {
			// An instance vanishing before its removal is fine: it is
			// what this function is working towards, and the concurrent
			// config application removes instances too.
			if strings.Contains(out, "Can't find BGP instance") {
				continue
			}
			return fmt.Errorf("failed to remove %q from the running config: %w (output: %q)",
				name, err, out)
		}
	}
	return nil
}

func waitBGPD(router openperouter.RouterExecutor) error {
	backoff := wait.Backoff{Steps: 60, Duration: time.Second}
	alwaysRetry := func(error) bool { return true }
	return retry.OnError(backoff, alwaysRetry, func() error {
		_, err := router.Exec("vtysh", "-d", "bgpd", "-c", "show bgp summary")
		return err
	})
}
