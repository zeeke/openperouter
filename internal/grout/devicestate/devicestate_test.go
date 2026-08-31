// SPDX-License-Identifier:Apache-2.0

package devicestate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveLoadDelete(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	state := Entry{
		InterfaceName:  "enp3s0f0v0",
		PCIAddress:     "0000:03:02.0",
		OriginalDriver: "ice",
		Addresses:      []string{"192.168.1.10/24", "fd00::10/64"},
	}

	require.NoError(t, Save(state))

	loaded, err := Load(Entry{InterfaceName: "enp3s0f0v0"})
	require.NoError(t, err)
	assert.Equal(t, state, *loaded)

	require.NoError(t, Delete(Entry{InterfaceName: "enp3s0f0v0"}))
	_, err = os.Stat(filePath(Entry{InterfaceName: "enp3s0f0v0"}))
	assert.True(t, os.IsNotExist(err), "state file should be removed")

	loaded, err = Load(Entry{InterfaceName: "enp3s0f0v0"})
	require.NoError(t, err)
	assert.Equal(t, "enp3s0f0v0", loaded.InterfaceName)
	assert.Empty(t, loaded.PCIAddress)
}

func TestOverwrite(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	require.NoError(t, Save(Entry{
		InterfaceName: "toswitch1",
		Addresses:     []string{"10.0.0.1/24"},
	}))
	require.NoError(t, Save(Entry{
		InterfaceName: "toswitch1",
		Addresses:     []string{"192.168.1.1/24", "fd00::1/64"},
	}))

	loaded, err := Load(Entry{InterfaceName: "toswitch1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1/24", "fd00::1/64"}, loaded.Addresses)
}

func TestDeleteNonExistent(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	assert.NoError(t, Delete(Entry{InterfaceName: "does_not_exist"}))
	assert.Error(t, Delete(Entry{}))
	assert.Error(t, Save(Entry{}))
}

func TestLoadByPCI(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	require.NoError(t, Save(Entry{
		PCIAddress:     "0000:01:00.0",
		InterfaceName:  "ens1f0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24"},
	}))
	require.NoError(t, Save(Entry{
		PCIAddress:     "0000:02:00.0",
		InterfaceName:  "ens2f0",
		OriginalDriver: "mlx5_core",
	}))
	require.NoError(t, Save(Entry{
		InterfaceName: "toswitch1",
		Addresses:     []string{"192.168.11.3/24"},
	}))

	t.Run("finds matching PCI address", func(t *testing.T) {
		loaded, err := LoadByPCI("0000:02:00.0")
		require.NoError(t, err)
		assert.Equal(t, "ens2f0", loaded.InterfaceName)
		assert.Equal(t, "mlx5_core", loaded.OriginalDriver)
	})

	t.Run("errors when PCI address is unknown", func(t *testing.T) {
		_, err := LoadByPCI("0000:ff:00.0")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no device state for PCI address")
	})

	t.Run("errors when PCI address is empty", func(t *testing.T) {
		_, err := LoadByPCI("")
		assert.Error(t, err)
	})
}
