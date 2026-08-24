// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"path/filepath"
	"testing"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroutPortToUnderlayInterface(t *testing.T) {
	origDir := devicestate.Dir
	devicestate.Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { devicestate.Dir = origDir })

	require.NoError(t, devicestate.Save(devicestate.Entry{
		PCIAddress:     "0000:01:00.0",
		InterfaceName:  "ens1f0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24"},
	}))

	t.Run("pci port loads interface name from device state", func(t *testing.T) {
		got, err := groutPortToUnderlayInterface(
			groutInterface{Name: "u_ens1f0"},
			&groutInterfaceDetails{
				Devargs:     "0000:01:00.0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "ens1f0", got.InterfaceName)
		assert.Equal(t, hostnetwork.UnderlayInterfaceNetDev, got.Kind)
		require.NotNil(t, got.AcceleratedConfig)
	})

	t.Run("pci port without device state returns error", func(t *testing.T) {
		_, err := groutPortToUnderlayInterface(
			groutInterface{Name: "u_missing"},
			&groutInterfaceDetails{
				Devargs:     "0000:ff:00.0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to load device state")
	})
}
