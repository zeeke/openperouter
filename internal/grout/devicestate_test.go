// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateFilePath(t *testing.T) {
	tests := []struct {
		name     string
		state    SavedDeviceState
		wantBase string
	}{
		{
			name:     "PCI address",
			state:    SavedDeviceState{PCIAddress: "0000:01:00.0"},
			wantBase: "0000-01-00.0.json",
		},
		{
			name:     "netlink name only",
			state:    SavedDeviceState{NetlinkName: "toswitch1"},
			wantBase: "net_toswitch1.json",
		},
		{
			name:     "PCI takes precedence over netlink",
			state:    SavedDeviceState{PCIAddress: "0000:02:00.0", NetlinkName: "eth0"},
			wantBase: "0000-02-00.0.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stateFilePath(tt.state)
			assert.Equal(t, tt.wantBase, filepath.Base(got))
		})
	}
}

func TestSaveLoadDeleteDeviceState_PCI(t *testing.T) {
	origDir := deviceStateDir
	deviceStateDir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { deviceStateDir = origDir })

	state := SavedDeviceState{
		PCIAddress:     "0000:01:00.0",
		NetlinkName:    "eth0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24", "fd00::1/64"},
	}

	require.NoError(t, saveDeviceState(state))

	loaded, err := loadDeviceState(SavedDeviceState{PCIAddress: "0000:01:00.0"})
	require.NoError(t, err)
	assert.Equal(t, state, *loaded)

	require.NoError(t, deleteDeviceState(SavedDeviceState{PCIAddress: "0000:01:00.0"}))
	_, err = loadDeviceState(SavedDeviceState{PCIAddress: "0000:01:00.0"})
	assert.Error(t, err)
}

func TestSaveLoadDeleteDeviceState_NetlinkName(t *testing.T) {
	origDir := deviceStateDir
	deviceStateDir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { deviceStateDir = origDir })

	state := SavedDeviceState{
		NetlinkName: "toswitch1",
		Addresses:   []string{"192.168.11.3/24", "2001:db8:11::3/64"},
	}

	require.NoError(t, saveDeviceState(state))

	path := stateFilePath(SavedDeviceState{NetlinkName: "toswitch1"})
	_, err := os.Stat(path)
	require.NoError(t, err, "state file should exist")

	loaded, err := loadDeviceState(SavedDeviceState{NetlinkName: "toswitch1"})
	require.NoError(t, err)
	assert.Equal(t, "toswitch1", loaded.NetlinkName)
	assert.Equal(t, []string{"192.168.11.3/24", "2001:db8:11::3/64"}, loaded.Addresses)

	require.NoError(t, deleteDeviceState(SavedDeviceState{NetlinkName: "toswitch1"}))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "state file should be removed")
}

func TestDeviceStateOverwrite(t *testing.T) {
	origDir := deviceStateDir
	deviceStateDir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { deviceStateDir = origDir })

	require.NoError(t, saveDeviceState(SavedDeviceState{
		NetlinkName: "toswitch2",
		Addresses:   []string{"10.0.0.1/24"},
	}))

	require.NoError(t, saveDeviceState(SavedDeviceState{
		NetlinkName: "toswitch2",
		Addresses:   []string{"192.168.1.1/24", "fd00::1/64"},
	}))

	loaded, err := loadDeviceState(SavedDeviceState{NetlinkName: "toswitch2"})
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1/24", "fd00::1/64"}, loaded.Addresses)
}

func TestDeleteDeviceStateNonExistent(t *testing.T) {
	origDir := deviceStateDir
	deviceStateDir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { deviceStateDir = origDir })

	err := deleteDeviceState(SavedDeviceState{NetlinkName: "does_not_exist"})
	assert.NoError(t, err)

	err = deleteDeviceState(SavedDeviceState{PCIAddress: "0000:ff:ff.f"})
	assert.NoError(t, err)
}
