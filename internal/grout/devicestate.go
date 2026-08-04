// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var deviceStateDir = "/var/run/openperouter/grout"

// SavedDeviceState records the original state of a PCI device before
// the DPDK driver is bound, so the device can be restored on teardown.
type SavedDeviceState struct {
	PCIAddress     string   `json:"pciAddress"`
	NetlinkName    string   `json:"netlinkName"`
	OriginalDriver string   `json:"originalDriver"`
	Addresses      []string `json:"addresses"`
}

func stateFilePath(pciAddr string) string {
	safe := strings.ReplaceAll(pciAddr, ":", "-")
	return filepath.Join(deviceStateDir, safe+".json")
}

func saveDeviceState(state SavedDeviceState) error {
	if err := os.MkdirAll(deviceStateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create device state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal device state: %w", err)
	}
	path := stateFilePath(state.PCIAddress)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write device state to %s: %w", path, err)
	}
	return nil
}

func loadDeviceState(pciAddr string) (*SavedDeviceState, error) {
	path := stateFilePath(pciAddr)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read device state from %s: %w", path, err)
	}
	var state SavedDeviceState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal device state from %s: %w", path, err)
	}
	return &state, nil
}

func deleteDeviceState(pciAddr string) error {
	path := stateFilePath(pciAddr)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete device state file %s: %w", path, err)
	}
	return nil
}
