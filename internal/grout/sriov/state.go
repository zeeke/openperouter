// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StateDir is the directory where VF state files are persisted.
// Override in tests to point at a temporary directory.
var StateDir = "/var/run/openperouter/sriov"

// VFState holds the initial configuration of a VF captured before it is
// bound to a DPDK driver. The kernel netdev disappears after binding, so
// this state is the only way to identify the device on subsequent
// reconciles or to restore the original driver.
type VFState struct {
	InitialLinkName string `json:"initialLinkName"`
	PCIAddress      string `json:"pciAddress"`
	InitialDriver   string `json:"initialDriver"`
}

func stateFilePath(stateDir, vfName string) string {
	return filepath.Join(stateDir, "vf_"+vfName)
}

// SaveVFState writes the VF state as JSON to <stateDir>/vf_<vfName>.
func SaveVFState(stateDir, vfName string, state VFState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal VF state for %s: %w", vfName, err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state directory %s: %w", stateDir, err)
	}
	path := stateFilePath(stateDir, vfName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write VF state file %s: %w", path, err)
	}
	return nil
}

// LoadVFState reads the VF state from <stateDir>/vf_<vfName>.
// Returns nil, nil if the file does not exist.
func LoadVFState(stateDir, vfName string) (*VFState, error) {
	path := stateFilePath(stateDir, vfName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read VF state file %s: %w", path, err)
	}
	var state VFState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse VF state file %s: %w", path, err)
	}
	return &state, nil
}

// RemoveVFState deletes the VF state file for the given VF name.
// Returns nil if the file does not exist.
func RemoveVFState(stateDir, vfName string) error {
	path := stateFilePath(stateDir, vfName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove VF state file %s: %w", path, err)
	}
	return nil
}
