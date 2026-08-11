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

// SavedDeviceState records the original state of a network device before
// it is handed to grout, so the device can be restored on teardown.
//
// For PCI/DPDK ports the PCIAddress is set and the state file is named
// after the BDF. For TAP-based underlay interfaces (NetDev/CNIDev) only
// the NetlinkName is set and the file is named "net_<NetlinkName>".
type SavedDeviceState struct {
	PCIAddress     string   `json:"pciAddress,omitempty"`
	NetlinkName    string   `json:"netlinkName,omitempty"`
	OriginalDriver string   `json:"originalDriver,omitempty"`
	Addresses      []string `json:"addresses"`
}

// stateFilePath derives the on-disk path for a SavedDeviceState.
// PCI devices use the BDF-escaped PCIAddress; TAP-based interfaces
// (no PCIAddress) use "net_<NetlinkName>".
func stateFilePath(state SavedDeviceState) string {
	var base string
	if state.PCIAddress != "" {
		base = strings.ReplaceAll(state.PCIAddress, ":", "-")
	} else {
		base = "net_" + state.NetlinkName
	}
	return filepath.Join(deviceStateDir, base+".json")
}

func saveDeviceState(state SavedDeviceState) error {
	if err := os.MkdirAll(deviceStateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create device state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal device state: %w", err)
	}
	path := stateFilePath(state)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write device state to %s: %w", path, err)
	}
	return nil
}

func loadDeviceState(key SavedDeviceState) (*SavedDeviceState, error) {
	path := stateFilePath(key)
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

func deleteDeviceState(key SavedDeviceState) error {
	path := stateFilePath(key)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete device state file %s: %w", path, err)
	}
	return nil
}
