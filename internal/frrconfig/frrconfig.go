// SPDX-License-Identifier:Apache-2.0

package frrconfig

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/openperouter/openperouter/internal/frr"
)

const (
	test         = "test"
	reload       = "reload"
	reloaderPath = "/usr/lib/frr/frr-reload.py"
	vtyshPath    = "/usr/bin/vtysh"

	// bgpdWaitTimeout bounds the wait for bgpd to answer on its vty. bgpd
	// normally answers immediately; the bound covers a bgpd that is coming
	// back after a crash, which watchfrr restarts within seconds - or after
	// one of its restart intervals (60s minimum on FRR 10.7) when it
	// crashed repeatedly.
	bgpdWaitTimeout  = 130 * time.Second
	bgpdPollInterval = 500 * time.Millisecond
)

// errASMismatch signals that frr-reload failed because the running bgpd holds
// a default BGP instance whose AS differs from the desired configuration's.
// bgpd cannot apply such a change live: the running instance cannot be
// deleted while dependent VRF instances exist, and a new AS cannot be created
// while the old instance is running. The only recovery is clearing the
// running BGP instances and letting the reload rebuild them.
var errASMismatch = errors.New("bgpd is running a different AS than the desired configuration")

// Update reloads the frr configuration at the given path.
func Update(path string) error {
	return update(path, reloadAction, runVtysh)
}

func update(
	path string,
	reloadAction func(path string, action string) error,
	vtyshRunner func(...string) (string, error),
) error {
	slog.Info("config update", "path", path)
	desired, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read the desired config %s: %w", path, err)
	}
	// frr-reload exits successfully even when bgpd is down, silently
	// dropping the whole BGP configuration. Wait for bgpd before reloading
	// so the config is actually applied.
	if needsBGPD(desired) {
		if err := waitForBGPD(vtyshRunner, bgpdWaitTimeout, bgpdPollInterval); err != nil {
			return err
		}
	}
	if err := clearStaleISIS(path, vtyshRunner); err != nil {
		return err
	}
	if err := applyConfig(path, reloadAction); err != nil {
		if !isASMismatchReloadFailure(err) {
			return err
		}
		return recoverASMismatch(path, reloadAction, vtyshRunner)
	}
	return nil
}

// needsBGPD reports whether the configuration drives bgpd. Only then is a
// missing bgpd a reason to hold off a reload.
func needsBGPD(config []byte) bool {
	return strings.Contains(string(config), "router bgp ")
}

// isASMismatchReloadFailure reports whether a failed reload is the known
// failure class where bgpd refuses the configuration because its default
// BGP instance runs a different AS than the desired one. The reloader wraps
// the errASMismatch sentinel for this condition; callers that surface
// frr-reload's raw output as error text (the docker integration test) are
// covered by scanning it for bgpd's diagnostics.
func isASMismatchReloadFailure(err error) bool {
	return errors.Is(err, errASMismatch) || isASMismatchFailure(err.Error())
}

func applyConfig(path string, reloadAction func(path string, action string) error) error {
	if err := reloadAction(path, test); err != nil {
		return err
	}
	return reloadAction(path, reload)
}

// recoverASMismatch converges a configuration whose underlay AS differs from
// the one running in bgpd. bgpd defers the deletion of a BGP instance to the
// end of the config session applying it, so no single frr-reload run can
// swap the AS of the default instance: the new `router bgp <asn>` is
// rejected while the old instance is still running. Clearing the running
// instances through separate vtysh sessions first lets the reload rebuild
// the whole BGP configuration from scratch. The BGP sessions reset, which an
// underlay AS change resets anyway.
func recoverASMismatch(
	path string,
	reloadAction func(path string, action string) error,
	vtyshRunner func(...string) (string, error),
) error {
	slog.Info("bgpd runs a different AS than the desired config, clearing the running BGP instances to recover", "path", path)
	if err := teardownBGPInstances(vtyshRunner); err != nil {
		return fmt.Errorf("failed to clear the running BGP instances during AS mismatch recovery: %w", err)
	}
	// The instance churn can race with frr-reload's trailing `vtysh write`,
	// which then fails a reload that actually applied. Retrying once turns
	// that into a successful no-op reload.
	if err := applyConfig(path, reloadAction); err != nil {
		slog.Warn("reload after clearing the BGP instances failed, retrying once", "error", err)
		if err := applyConfig(path, reloadAction); err != nil {
			return fmt.Errorf("failed to reload after clearing the BGP instances: %w", err)
		}
	}
	slog.Info("BGP instances rebuilt with the desired config, reload converged")
	return nil
}

// teardownBGPInstances removes every BGP instance from the running config,
// each through its own vtysh session so the deletion is committed before the
// next one runs. The desired configuration's reload rebuilds them.
func teardownBGPInstances(vtyshRunner func(...string) (string, error)) error {
	running, err := vtyshRunner("show running-config")
	if err != nil {
		return fmt.Errorf("failed to read the running config: %w", err)
	}
	commands := bgpInstanceTeardownCommands(running)
	if len(commands) == 0 {
		return nil
	}
	slog.Info("clearing the running BGP instances before the reload", "commands", commands)
	for _, command := range commands {
		if _, err := vtyshRunner("configure terminal", command, "end"); err != nil {
			return fmt.Errorf("failed to clear the BGP instance %q: %w", command, err)
		}
	}
	return nil
}

// bgpInstanceTeardownCommands returns the `no router bgp` commands that
// remove every BGP instance declared in the given running config, VRF
// instances first: bgpd refuses to delete the default instance while
// dependent VRF instances exist.
func bgpInstanceTeardownCommands(runningConfig string) []string {
	var vrfInstances, defaultInstance []string
	for line := range strings.SplitSeq(runningConfig, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && fields[0] == "router" && fields[1] == "bgp":
			defaultInstance = []string{"no " + strings.Join(fields, " ")}
		case len(fields) == 5 && fields[0] == "router" && fields[1] == "bgp" && fields[3] == "vrf":
			vrfInstances = append(vrfInstances, "no "+strings.Join(fields, " "))
		}
	}
	return append(vrfInstances, defaultInstance...)
}

// waitForBGPD waits until bgpd answers on its vty, so a reload is not
// silently applied while bgpd is down.
func waitForBGPD(vtyshRunner func(...string) (string, error), timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		output, err := vtyshRunner("show daemons")
		if err == nil && strings.Contains(output, "bgpd") {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"bgpd did not answer within %s: last error: %v, last output: %q",
				timeout, err, frr.RedactPasswords(output),
			)
		}
		time.Sleep(pollInterval) //nolint:forbidigo // poll interval of the bounded wait
	}
}

var execCommand = exec.Command

func reloadAction(path string, action string) error {
	reloadParameter := "--" + action
	cmd := execCommand("python3", reloaderPath, reloadParameter, "--logfile", "/dev/null", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("frr update failed", "action", action, "error", err, "output", frr.RedactPasswords(string(output)))
		if isASMismatchFailure(string(output)) {
			return fmt.Errorf("frr update %s failed: %w", action, errASMismatch)
		}
		return fmt.Errorf("frr update %s failed: %w", action, err)
	}
	slog.Debug("frr update succeeded", "action", action, "output", frr.RedactPasswords(string(output)))
	return nil
}

// isASMismatchFailure reports whether the frr-reload output is the known
// failure class where bgpd refuses the reload because its default BGP
// instance runs a different AS than the desired configuration. The match is
// deliberately narrow: only bgpd's own diagnostics for this condition
// qualify, so unrelated reload failures never trigger a bgpd restart.
func isASMismatchFailure(output string) bool {
	return strings.Contains(output, "BGP instance is already running; AS is") ||
		strings.Contains(output, "BGP instance name and AS number mismatch")
}

func runVtysh(commands ...string) (string, error) {
	args := make([]string, 0, len(commands)*2)
	for _, c := range commands {
		args = append(args, "-c", c)
	}
	cmd := execCommand(vtyshPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"vtysh command failed: cmd: %q, output: %q, err: %w",
			frr.RedactPasswords(strings.Join(commands, "\n")),
			frr.RedactPasswords(string(output)),
			err,
		)
	}
	slog.Debug(
		"frr update ran vtysh command",
		"command", frr.RedactPasswords(strings.Join(commands, "\n")),
		"output", frr.RedactPasswords(string(output)),
	)
	return string(output), nil
}

// clearStaleISIS works around an frr-reload.py ordering bug hit when an `isis passive` is
// removed: frr-reload emits "no isis passive" on an interface after it has
// already detached that interface from the ISIS instance, and FRR rejects the
// command with a YANG "area-tag" error, aborting the reload. Clearing the
// interface-level passive setting while the ISIS instance still exists
// fixes the issue, leading reloads to succeed. See openperouter issue #645
// and upstream FRR issue https://github.com/FRRouting/frr/issues/10133.
func clearStaleISIS(desiredConfigPath string, vtyshRunner func(...string) (string, error)) error {
	running, err := vtyshRunner("show running-config")
	if err != nil {
		return fmt.Errorf("failed to read running config: %w", err)
	}

	runningPassiveInterfaces := isisPassiveInterfaces(running)
	// Nothing to do if we have no running passive interfaces.
	if len(runningPassiveInterfaces) == 0 {
		return nil
	}

	desired, err := os.ReadFile(desiredConfigPath)
	if err != nil {
		return fmt.Errorf("failed to read desired config %s: %w", desiredConfigPath, err)
	}
	desiredPassiveInterfaces := isisPassiveInterfaces(string(desired))

	passiveInterfacesToRemove := map[string]struct{}{}
	for runningPassiveInterface := range runningPassiveInterfaces {
		if _, found := desiredPassiveInterfaces[runningPassiveInterface]; !found {
			passiveInterfacesToRemove[runningPassiveInterface] = struct{}{}
		}
	}

	if len(passiveInterfacesToRemove) == 0 {
		return nil
	}

	commands := isisTeardownCommands(passiveInterfacesToRemove)
	slog.Info("clearing stale ISIS state before reload", "commands", commands)
	if _, err := vtyshRunner(commands...); err != nil {
		return fmt.Errorf("failed to clear stale ISIS state: %w", err)
	}
	return nil
}

func isisPassiveInterfaces(config string) map[string]struct{} {
	passiveInterfaces := map[string]struct{}{}
	currentInterface := ""

	for line := range strings.SplitSeq(config, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "interface "):
			currentInterface = strings.TrimPrefix(trimmed, "interface ")
		case trimmed == "isis passive" && currentInterface != "":
			passiveInterfaces[currentInterface] = struct{}{}
		case trimmed == "exit" || trimmed == "end":
			currentInterface = ""
		}
	}
	return passiveInterfaces
}

func isisTeardownCommands(passiveInterfaces map[string]struct{}) []string {
	commands := make([]string, 0, 2+3*len(passiveInterfaces))
	commands = append(commands, "configure terminal")
	for iface := range passiveInterfaces {
		commands = append(commands,
			fmt.Sprintf("interface %s", iface),
			"no isis passive",
			"exit",
		)
	}
	commands = append(commands, "end")
	return commands
}
