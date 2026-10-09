// SPDX-License-Identifier:Apache-2.0

package devicestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// dir is the directory that holds per-device state files. It can be
// overridden in tests.
var dir = "/var/run/grout/device-state"
var ErrDeviceStateNotFound = errors.New("device state not found")

// Entry records the original state of a network device before it is
// handed to grout, so the device can be restored on teardown.
//
// InterfaceName is the primary deviceID and the state file is named
// "<interfaceName>.json". PCIAddress is stored in the file so a DPDK
// port can be mapped back to its original kernel interface.
type Entry struct {
	InterfaceName  string   `json:"interfaceName"`
	PCIAddress     string   `json:"pciAddress,omitempty"`
	OriginalDriver string   `json:"originalDriver,omitempty"`
	Addresses      []string `json:"addresses"`
	MTU            int32    `json:"mtu,omitempty"`
	// PortName is the grout port created for the device, so a teardown
	// interrupted after the port was deleted can still be resumed.
	PortName string `json:"portName,omitempty"`
}

func Save(deviceID string, state Entry) error {
	if deviceID == "" {
		return fmt.Errorf("device state deviceID is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create device state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal device state: %w", err)
	}
	path := filePath(deviceID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write device state to %s: %w", path, err)
	}
	return nil
}

func Load(deviceID string) (*Entry, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("device state deviceID is required")
	}
	return loadEntry(filePath(deviceID))
}

// List returns all the saved device states.
func List() ([]Entry, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to list device state files in %s: %w", dir, err)
	}
	entries := make([]Entry, 0, len(paths))
	for _, path := range paths {
		entry, err := loadEntry(path)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

func Delete(deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("device state deviceID is required")
	}
	path := filePath(deviceID)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to delete device state file %s: %w", path, err)
	}
	return nil
}

func filePath(deviceID string) string {
	return filepath.Join(dir, deviceID+".json")
}

func loadEntry(path string) (*Entry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrDeviceStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read device state from %s: %w", path, err)
	}
	var state Entry
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal device state from %s: %w", path, err)
	}
	return &state, nil
}
