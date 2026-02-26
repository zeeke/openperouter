// SPDX-License-Identifier:Apache-2.0

// Package grout provides a client for managing grout DPDK dataplane interfaces.
// It communicates with the grout daemon via grcli commands over the UNIX socket.
package grout

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Client communicates with the grout daemon via grcli.
type Client struct {
	socketPath string
}

// NewClient creates a new grout client pointing at the given UNIX socket.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// EnsurePort creates a grout port interface with the given name and devargs
// if it does not already exist. It is idempotent.
func (c *Client) EnsurePort(ctx context.Context, name, devargs string) error {
	exists, err := c.portExists(ctx, name)
	if err != nil {
		return fmt.Errorf("checking if port %s exists: %w", name, err)
	}
	if exists {
		slog.InfoContext(ctx, "grout port already exists", "name", name)
		return nil
	}

	slog.InfoContext(ctx, "creating grout port", "name", name, "devargs", devargs)
	if err := c.run(ctx, "interface", "add", "port", name, "devargs", devargs); err != nil {
		return fmt.Errorf("creating grout port %s: %w", name, err)
	}
	return nil
}

// DeletePort removes a grout port interface by name.
// It is idempotent: if the port does not exist, it returns nil.
func (c *Client) DeletePort(ctx context.Context, name string) error {
	exists, err := c.portExists(ctx, name)
	if err != nil {
		return fmt.Errorf("checking if port %s exists: %w", name, err)
	}
	if !exists {
		return nil
	}

	slog.InfoContext(ctx, "deleting grout port", "name", name)
	if err := c.run(ctx, "interface", "del", name); err != nil {
		return fmt.Errorf("deleting grout port %s: %w", name, err)
	}
	return nil
}

// portExists checks whether a port with the given name exists in grout.
func (c *Client) portExists(ctx context.Context, name string) (bool, error) {
	out, err := c.runOutput(ctx, "interface", "show", "name", name)
	if err != nil {
		// grcli returns an error when the interface doesn't exist
		if strings.Contains(err.Error(), "No such") || strings.Contains(out, "No such") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UnderlayDevargs returns DPDK devargs for a TAP interface mirroring a kernel NIC.
// The remote parameter causes DPDK to redirect traffic from the kernel interface.
func UnderlayDevargs(nicName string) string {
	return fmt.Sprintf("net_tap0,iface=gr-%s,remote=%s", nicName, nicName)
}

// PassthroughTAPName is the kernel interface name for the grout passthrough TAP device.
// In grout mode, this TAP replaces the veth pair used in kernel mode.
const PassthroughTAPName = "gr-pt"

// PassthroughPortName is the grout port name for the passthrough TAP.
const PassthroughPortName = "pt"

// PassthroughDevargs returns DPDK devargs for a TAP interface used as the
// passthrough endpoint. Unlike the underlay, no `remote` is specified because
// the TAP itself IS the forwarding interface (there is no kernel interface to mirror).
func PassthroughDevargs() string {
	return fmt.Sprintf("net_tap1,iface=%s", PassthroughTAPName)
}

// run executes a grcli command and returns any error.
func (c *Client) run(ctx context.Context, args ...string) error {
	_, err := c.runOutput(ctx, args...)
	return err
}

// runOutput executes a grcli command and returns stdout and any error.
func (c *Client) runOutput(ctx context.Context, args ...string) (string, error) {
	cmdArgs := append([]string{"-e", "-s", c.socketPath}, args...)
	cmd := exec.CommandContext(ctx, "grcli", cmdArgs...)

	slog.DebugContext(ctx, "running grcli", "args", cmdArgs)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if err != nil {
		return output, fmt.Errorf("grcli %s failed: %w, output: %s", strings.Join(args, " "), err, output)
	}
	return output, nil
}
