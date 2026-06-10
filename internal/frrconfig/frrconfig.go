// SPDX-License-Identifier:Apache-2.0

package frrconfig

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const bgpdPidFile = "/var/run/frr/bgpd.pid"

type Action string

const (
	Test         Action = "test"
	Reload       Action = "reload"
	reloaderPath        = "/usr/lib/frr/frr-reload.py"
)

// Update reloads the frr configuration at the given path.
func Update(path string) error {
	slog.Info("config update", "path", path)
	err := reloadAction(path, Test)
	if err != nil {
		return err
	}
	err = reloadAction(path, Reload)
	if err != nil {
		return err
	}
	return nil
}

const reloadTimeout = 60 * time.Second

var execCommand = exec.CommandContext

func reloadAction(path string, action Action) error {
	reloadParameter := "--" + string(action)
	ctx, cancel := context.WithTimeout(context.Background(), reloadTimeout)
	defer cancel()
	cmd := execCommand(ctx, "python3", reloaderPath, reloadParameter, path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("frr update failed", "action", action, "error", err, "output", string(output))
		if ctx.Err() == context.DeadlineExceeded {
			slog.Error("frr-reload.py timed out, killing mgmtd to release lock")
			killMgmtd()
		}
		if action == Reload && isBgpdDown() {
			slog.Error("bgpd is not running, watchfrr will restart it (max 30s backoff)")
			cleanStaleBgpdPid()
		}
		return fmt.Errorf("frr update %s failed: %w", action, err)
	}
	slog.Debug("frr update succeeded", "action", action, "output", string(output))
	return nil
}

func isBgpdDown() bool {
	cmd := exec.Command("vtysh", "-c", "show daemons")
	output, err := cmd.Output()
	if err != nil {
		return true
	}
	return !strings.Contains(string(output), "bgpd")
}

func cleanStaleBgpdPid() {
	if err := os.Remove(bgpdPidFile); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove stale bgpd pid file", "error", err)
	}
}

func killMgmtd() {
	killByName("mgmtd", syscall.SIGKILL)
}

func killByName(name string, sig syscall.Signal) {
	pids, err := findPidsByName(name)
	if err != nil {
		slog.Warn("failed to find process", "name", name, "error", err)
		return
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, sig); err != nil {
			slog.Warn("failed to kill process", "name", name, "pid", pid, "error", err)
		} else {
			slog.Info("killed process", "name", name, "pid", pid, "signal", sig)
		}
	}
}

func findPidsByName(name string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == name {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
