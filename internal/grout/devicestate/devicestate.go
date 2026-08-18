// SPDX-License-Identifier:Apache-2.0

package devicestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

var Dir = "/var/run/openperouter/grout"

// Entry records the original state of a network device before
// it is handed to grout, so the device can be restored on teardown.
//
// NetlinkName is the primary key and the state file is named
// "<NetlinkName>.json". PCIAddress is stored in the file so a DPDK
// port can be mapped back to its original kernel interface.
type Entry struct {
	PCIAddress     string   `json:"pciAddress,omitempty"`
	NetlinkName    string   `json:"netlinkName,omitempty"`
	OriginalDriver string   `json:"originalDriver,omitempty"`
	Addresses      []string `json:"addresses"`
}

func filePath(state Entry) string {
	return filepath.Join(Dir, state.NetlinkName+".json")
}

func Save(state Entry) error {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return fmt.Errorf("failed to create device state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal device state: %w", err)
	}
	if state.NetlinkName == "" {
		return fmt.Errorf("netlink name is required")
	}
	path := filePath(state)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write device state to %s: %w", path, err)
	}
	return nil
}

func Load(key Entry) (*Entry, error) {
	path := filePath(key)
	if key.NetlinkName == "" {
		return nil, fmt.Errorf("netlink name is required")
	}

	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Entry{NetlinkName: key.NetlinkName}, nil
		}
		return nil, fmt.Errorf("failed to stat device state file %s: %w", path, err)
	}

	return loadFile(path)
}

// LoadByPCI finds the saved device state whose PCIAddress matches.
// State files are keyed by netlink name, so this scans the state directory.
func LoadByPCI(pciAddress string) (*Entry, error) {
	if pciAddress == "" {
		return nil, fmt.Errorf("pci address is required")
	}
	entries, err := os.ReadDir(Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list device state directory %s: %w", Dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		state, err := loadFile(filepath.Join(Dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if state.PCIAddress == pciAddress {
			return state, nil
		}
	}
	return nil, fmt.Errorf("no device state for PCI address %s", pciAddress)
}

func loadFile(path string) (*Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read device state from %s: %w", path, err)
	}
	var state Entry
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal device state from %s: %w", path, err)
	}
	return &state, nil
}

func Delete(key Entry) error {
	if key.NetlinkName == "" {
		return fmt.Errorf("netlink name is required")
	}
	path := filePath(key)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete device state file %s: %w", path, err)
	}
	return nil
}
