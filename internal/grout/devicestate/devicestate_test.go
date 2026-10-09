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
	origDir := dir
	dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { dir = origDir })

	state := Entry{
		InterfaceName:  "enp3s0f0v0",
		PCIAddress:     "0000:03:02.0",
		OriginalDriver: "ice",
		Addresses:      []string{"192.168.1.10/24", "fd00::10/64"},
		MTU:            9000,
	}

	require.NoError(t, Save("enp3s0f0v0", state))

	loaded, err := Load("enp3s0f0v0")
	require.NoError(t, err)
	assert.Equal(t, state, *loaded)

	require.NoError(t, Delete("enp3s0f0v0"))
	_, err = os.Stat(filePath("enp3s0f0v0"))
	assert.True(t, os.IsNotExist(err), "state file should be removed")

	_, err = Load("enp3s0f0v0")
	require.ErrorIs(t, err, ErrDeviceStateNotFound)
}

func TestOverwrite(t *testing.T) {
	origDir := dir
	dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { dir = origDir })

	require.NoError(t, Save("toswitch1", Entry{
		InterfaceName: "toswitch1",
		Addresses:     []string{"10.0.0.1/24"},
	}))
	require.NoError(t, Save("toswitch1", Entry{
		InterfaceName: "toswitch1",
		Addresses:     []string{"192.168.1.1/24", "fd00::1/64"},
	}))

	loaded, err := Load("toswitch1")
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1/24", "fd00::1/64"}, loaded.Addresses)
}

func TestDeleteNonExistent(t *testing.T) {
	origDir := dir
	dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { dir = origDir })

	assert.NoError(t, Delete("does_not_exist"))
	assert.Error(t, Delete(""))
	assert.Error(t, Save("", Entry{}))
	_, err := Load("")
	assert.Error(t, err)
}

func TestList(t *testing.T) {
	origDir := dir
	dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { dir = origDir })

	entries, err := List()
	require.NoError(t, err)
	assert.Empty(t, entries)

	tap := Entry{InterfaceName: "toswitch1", Addresses: []string{"10.0.0.1/24"}, PortName: "u_toswitch1"}
	pci := Entry{InterfaceName: "ens1f0", PCIAddress: "0000:03:02.0", OriginalDriver: "ice", PortName: "p0"}
	require.NoError(t, Save(tap.InterfaceName, tap))
	require.NoError(t, Save(pci.InterfaceName, pci))

	entries, err = List()
	require.NoError(t, err)
	assert.ElementsMatch(t, []Entry{tap, pci}, entries)
}
