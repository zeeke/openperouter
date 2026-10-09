// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/pci"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

const testPCIAddr = "0000:03:00.0"

func TestSetupInterfaceForHWAcceleration(t *testing.T) {
	tests := []struct {
		name            string
		driver          string
		vfioLoaded      bool
		expectOverride  string
		expectUnbind    bool
		expectProbe     bool
		expectErrSubstr string
	}{
		{
			name:       "already bound to vfio-pci is a no-op",
			driver:     pci.DriverVFIOPCI,
			vfioLoaded: true,
		},
		{
			name:           "kernel driver is rebound to vfio-pci",
			driver:         "igb",
			vfioLoaded:     true,
			expectOverride: pci.DriverVFIOPCI,
			expectUnbind:   true,
			expectProbe:    true,
		},
		{
			name:           "unbound device is bound to vfio-pci",
			vfioLoaded:     true,
			expectOverride: pci.DriverVFIOPCI,
			expectProbe:    true,
		},
		{
			name:            "vfio-pci module not loaded",
			driver:          "igb",
			expectErrSubstr: "vfio-pci driver is not loaded",
		},
		{
			name:            "mlx5 without a kernel netdev",
			driver:          pci.DriverMlx5Core,
			vfioLoaded:      true,
			expectErrSubstr: "failed to move mlx5 netlink device nonexistent0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sysfs := newFakeSysfs(t, testPCIAddr, tc.driver)
			if tc.vfioLoaded {
				sysfs.addDriver(pci.DriverVFIOPCI)
			}

			err := setupInterfaceForHWAcceleration(context.Background(), netns.None(), testPCIAddr, "nonexistent0")
			if tc.expectErrSubstr != "" {
				require.ErrorContains(t, err, tc.expectErrSubstr)
				return
			}
			require.NoError(t, err)

			assert.Equal(t, tc.expectOverride, sysfs.read(sysfs.devicePath("driver_override")))
			if tc.expectUnbind {
				assert.Equal(t, testPCIAddr, sysfs.read(sysfs.driverPath(tc.driver, "unbind")))
			}
			if tc.expectProbe {
				assert.Equal(t, testPCIAddr, sysfs.read(filepath.Join(sysfs.root, "bus", "pci", "drivers_probe")))
			}
		})
	}
}

func TestConfigureAcceleratedPort(t *testing.T) {
	t.Run("creates a PCI port with the saved MTU and addresses", func(t *testing.T) {
		calls := recordCmdExec(t,
			cmdCall{
				cmd:    "grcli --err-exit --json --socket sock interface show name u_ens1f0",
				output: interfaceNotFoundOutput,
				err:    fmt.Errorf("exit status 1"),
			},
			cmdCall{
				cmd: "grcli --err-exit --json --socket sock interface add port u_ens1f0 devargs " + testPCIAddr +
					" mtu 9000 description underlay-for=ens1f0",
			},
			// Host routes need no kernel connected route, so no netlink access is required.
			cmdCall{cmd: "grcli --err-exit --json --socket sock address add 192.168.11.3/32 iface u_ens1f0"},
			cmdCall{cmd: "grcli --err-exit --json --socket sock address add 2001:db8::3/128 iface u_ens1f0"},
		)

		iface := hostnetwork.UnderlayInterface{InterfaceName: "ens1f0", Kind: hostnetwork.UnderlayInterfaceNetDev}
		state := &devicestate.Entry{
			InterfaceName: "ens1f0",
			PCIAddress:    testPCIAddr,
			MTU:           9000,
			Addresses:     []string{"192.168.11.3/32", "2001:db8::3/128"},
		}
		require.NoError(t, configureAcceleratedPort(context.Background(), NewClient("sock"), iface, state))
		assert.Len(t, *calls, 4)
	})

	t.Run("applies the accelerated config options", func(t *testing.T) {
		calls := recordCmdExec(t,
			cmdCall{
				cmd:    "grcli --err-exit --json --socket sock interface show name p0",
				output: interfaceNotFoundOutput,
				err:    fmt.Errorf("exit status 1"),
			},
			cmdCall{
				cmd: "grcli --err-exit --json --socket sock interface add port p0 devargs " + testPCIAddr +
					" mtu 1500 rxqs 4 qsize 1024 description underlay-for=ens1f0",
			},
		)

		iface := hostnetwork.UnderlayInterface{
			InterfaceName: "ens1f0",
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
				PortName: new("p0"),
				RXQueues: new(int32(4)),
				QSize:    new(int32(1024)),
			},
		}
		state := &devicestate.Entry{InterfaceName: "ens1f0", PCIAddress: testPCIAddr, MTU: 1500}
		require.NoError(t, configureAcceleratedPort(context.Background(), NewClient("sock"), iface, state))
		assert.Len(t, *calls, 2)
	})

	t.Run("existing matching port is kept", func(t *testing.T) {
		calls := recordCmdExec(t,
			cmdCall{
				cmd: "grcli --err-exit --json --socket sock interface show name u_ens1f0",
				output: `{"name":"u_ens1f0","type":"port","devargs":"` + testPCIAddr +
					`","mtu":1500,"description":"underlay-for=ens1f0"}`,
			},
		)

		iface := hostnetwork.UnderlayInterface{InterfaceName: "ens1f0", Kind: hostnetwork.UnderlayInterfaceNetDev}
		state := &devicestate.Entry{InterfaceName: "ens1f0", PCIAddress: testPCIAddr, MTU: 1500}
		require.NoError(t, configureAcceleratedPort(context.Background(), NewClient("sock"), iface, state))
		assert.Len(t, *calls, 1)
	})
}

func TestRestoreAcceleratedDeviceWithoutRebind(t *testing.T) {
	for _, originalDriver := range []string{"", pci.DriverVFIOPCI} {
		t.Run(fmt.Sprintf("original driver %q", originalDriver), func(t *testing.T) {
			// An empty sysfs makes any driver operation fail.
			origRoot := pci.SysfsRoot
			t.Cleanup(func() { pci.SysfsRoot = origRoot })
			pci.SysfsRoot = t.TempDir()

			state := &devicestate.Entry{InterfaceName: "ens1f0", PCIAddress: testPCIAddr, OriginalDriver: originalDriver}
			require.NoError(t, restoreAcceleratedDevice(context.Background(), "/var/run/netns/unused", state))
		})
	}
}

func TestRestoreAcceleratedDeviceFailsWhenRebindFails(t *testing.T) {
	sysfs := newFakeSysfs(t, testPCIAddr, pci.DriverVFIOPCI)
	// The original driver is not present in sysfs, so binding it fails.
	state := &devicestate.Entry{InterfaceName: "ens1f0", PCIAddress: testPCIAddr, OriginalDriver: "igb"}

	err := restoreAcceleratedDevice(context.Background(), "/var/run/netns/unused", state)
	require.ErrorContains(t, err, "failed to restore driver igb")
	assert.Equal(t, testPCIAddr, sysfs.read(sysfs.driverPath(pci.DriverVFIOPCI, "unbind")))
}

func TestDeviceStateForAcceleratedDeviceRejectsNonPCIDevices(t *testing.T) {
	sysfs := newFakeSysfs(t, testPCIAddr, "igb")
	sysfs.addNetClassDevice("veth0", "../../devices/virtual/net/veth0")

	_, err := deviceStateForAcceleratedDevice("veth0")
	require.ErrorContains(t, err, "does not look like a PCI address")

	_, err = deviceStateForAcceleratedDevice("missing0")
	require.ErrorContains(t, err, "failed to resolve PCI address for missing0")
}

// fakeSysfs is a minimal /sys tree holding one PCI device, enough for the pci
// package to read and change the bound driver.
type fakeSysfs struct {
	t       *testing.T
	root    string
	pciAddr string
}

func newFakeSysfs(t *testing.T, pciAddr, driver string) *fakeSysfs {
	t.Helper()
	origRoot := pci.SysfsRoot
	t.Cleanup(func() { pci.SysfsRoot = origRoot })
	pci.SysfsRoot = t.TempDir()

	s := &fakeSysfs{t: t, root: pci.SysfsRoot, pciAddr: pciAddr}
	s.mkdir(s.devicePath())
	s.write(filepath.Join(s.root, "bus", "pci", "drivers_probe"), "")
	if driver != "" {
		s.bind(driver)
	}
	return s
}

// bind points the device "driver" symlink at the given driver, as the kernel
// does once a driver probes the device.
func (s *fakeSysfs) bind(driver string) {
	s.t.Helper()
	s.addDriver(driver)
	link := s.devicePath("driver")
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
	}
	if err := os.Symlink(s.driverPath(driver), link); err != nil {
		s.t.Fatal(err)
	}
}

func (s *fakeSysfs) addDriver(driver string) {
	s.t.Helper()
	s.mkdir(s.driverPath(driver))
	s.write(s.driverPath(driver, "bind"), "")
	s.write(s.driverPath(driver, "unbind"), "")
}

func (s *fakeSysfs) addNetClassDevice(name, deviceTarget string) {
	s.t.Helper()
	dir := filepath.Join(s.root, "class", "net", name)
	s.mkdir(dir)
	if err := os.Symlink(deviceTarget, filepath.Join(dir, "device")); err != nil {
		s.t.Fatal(err)
	}
}

func (s *fakeSysfs) devicePath(elem ...string) string {
	return filepath.Join(append([]string{s.root, "bus", "pci", "devices", s.pciAddr}, elem...)...)
}

func (s *fakeSysfs) driverPath(driver string, elem ...string) string {
	return filepath.Join(append([]string{s.root, "bus", "pci", "drivers", driver}, elem...)...)
}

func (s *fakeSysfs) read(path string) string {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return string(data)
}

func (s *fakeSysfs) write(path, content string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *fakeSysfs) mkdir(path string) {
	s.t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		s.t.Fatal(err)
	}
}

// recordCmdExec mocks grcli like mockCmdExec and records the commands that
// were run, so a test can assert which grout calls happened.
func recordCmdExec(t *testing.T, cmdCalls ...cmdCall) *[]string {
	t.Helper()
	t.Cleanup(mockCmdExec(cmdCalls...))
	mocked := execCmd
	var calls []string
	execCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return mocked(ctx, name, args...)
	}
	return &calls
}
