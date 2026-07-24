// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var IntelKernelDrivers = map[string]bool{
	"igb":  true,
	"iavf": true,
	"ice":  true,
	"i40e": true,
}

const (
	DriverVFIOPCI  = "vfio-pci"
	DriverMlx5Core = "mlx5_core"
)

// GetPCIDriver returns the name of the kernel driver currently bound to
// the given PCI device. It reads the "driver" symlink under the device's
// sysfs directory. If no driver is bound the returned string is empty.
func GetPCIDriver(pciAddr string) (string, error) {
	driverLink := filepath.Join(SysfsRoot, "bus", "pci", "devices", pciAddr, "driver")
	target, err := os.Readlink(driverLink)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read driver for PCI device %s: %w", pciAddr, err)
	}
	return filepath.Base(target), nil
}

// GetPCINetDevice returns the name of the kernel network interface
// backed by the given PCI device. It reads the entries under the
// "net/" directory in the device's sysfs path.
func GetPCINetDevice(pciAddr string) (string, error) {
	netDir := filepath.Join(SysfsRoot, "bus", "pci", "devices", pciAddr, "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return "", fmt.Errorf("no kernel net device for PCI device %s: %w", pciAddr, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no kernel net device found under %s", netDir)
}

// BindVFIOPCI rebinds a PCI device to the vfio-pci driver.
// It is a no-op if the device is already bound to vfio-pci.
func BindVFIOPCI(pciAddr string) error {
	current, err := GetPCIDriver(pciAddr)
	if err != nil {
		return err
	}
	if current == DriverVFIOPCI {
		return nil
	}

	devicePath := filepath.Join(SysfsRoot, "bus", "pci", "devices", pciAddr)

	if err := os.WriteFile(filepath.Join(devicePath, "driver_override"),
		[]byte(DriverVFIOPCI), 0o644); err != nil {
		return fmt.Errorf("failed to set driver_override to vfio-pci for %s: %w", pciAddr, err)
	}

	if current != "" {
		unbindPath := filepath.Join(devicePath, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(pciAddr), 0o644); err != nil {
			return fmt.Errorf("failed to unbind driver %s from %s: %w", current, pciAddr, err)
		}
	}

	probePath := filepath.Join(SysfsRoot, "bus", "pci", "drivers_probe")
	if err := os.WriteFile(probePath, []byte(pciAddr), 0o644); err != nil {
		return fmt.Errorf("failed to probe driver for %s: %w", pciAddr, err)
	}

	return nil
}
