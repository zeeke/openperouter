// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var pciAddressRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// SysfsRoot can be overridden in tests.
var SysfsRoot = "/sys"

// ResolvePCIAddress validates the PCI address format and checks that the
// device exists in sysfs.
func ResolvePCIAddress(pciAddr string) error {
	if !pciAddressRegex.MatchString(pciAddr) {
		return fmt.Errorf("invalid PCI address format %q, expected DDDD:BB:DD.F", pciAddr)
	}
	devicePath := filepath.Join(SysfsRoot, "bus", "pci", "devices", pciAddr)
	if _, err := os.Stat(devicePath); err != nil {
		return fmt.Errorf("PCI device %q not found in sysfs: %w", pciAddr, err)
	}
	return nil
}

// ResolvePFVFIndex resolves a PF name and VF index to a PCI address by
// reading the virtfn symlink under the PF's sysfs device directory.
func ResolvePFVFIndex(pfName string, vfIndex int) (string, error) {
	virtfnLink := filepath.Join(SysfsRoot, "class", "net", pfName, "device", fmt.Sprintf("virtfn%d", vfIndex))
	target, err := os.Readlink(virtfnLink)
	if err != nil {
		return "", fmt.Errorf("failed to resolve VF %d on PF %q: %w", vfIndex, pfName, err)
	}
	pciAddr := filepath.Base(target)
	if !pciAddressRegex.MatchString(pciAddr) {
		return "", fmt.Errorf("resolved VF symlink target %q does not look like a PCI address", pciAddr)
	}
	return pciAddr, nil
}
