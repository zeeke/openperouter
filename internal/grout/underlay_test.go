// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"testing"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPortName(t *testing.T) {
	t.Run("default prefix", func(t *testing.T) {
		assert.Equal(t, "u_ens1f0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName: "ens1f0",
		}))
	})

	t.Run("accelerated without override still uses prefix", func(t *testing.T) {
		assert.Equal(t, "u_ens1f0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName:     "ens1f0",
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{},
		}))
	})

	t.Run("port name override", func(t *testing.T) {
		assert.Equal(t, "p0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName: "ens1f0",
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
				PortName: new("p0"),
			},
		}))
	})
}

func TestGroutPortToUnderlayInterface(t *testing.T) {
	t.Run("port uses interface name in description", func(t *testing.T) {
		got, isUnderlay, err := groutPortToUnderlayInterface(&groutInterfaceProperties{
			Name:        "tap-port",
			Devargs:     "net_tap0,iface=tap_eth0",
			Description: UnderlayInterfaceDescriptionPrefix + "eth0",
		})
		require.NoError(t, err)
		assert.True(t, isUnderlay)
		assert.Equal(t, "eth0", got.InterfaceName)
		assert.Equal(t, hostnetwork.UnderlayInterfaceNetDev, got.Kind)
		assert.Equal(t, "tap-port", PortName(got))
	})

	t.Run("pci port uses interface name in description", func(t *testing.T) {
		got, isUnderlay, err := groutPortToUnderlayInterface(&groutInterfaceProperties{
			Name:        "pci-port",
			Devargs:     "0000:ff:00.0",
			Description: UnderlayInterfaceDescriptionPrefix + "enp1s0",
		})
		require.NoError(t, err)
		assert.True(t, isUnderlay)
		assert.Equal(t, "enp1s0", got.InterfaceName)
		assert.Equal(t, "pci-port", PortName(got))
	})

	t.Run("port without underlay description is skipped", func(t *testing.T) {
		_, isUnderlay, err := groutPortToUnderlayInterface(&groutInterfaceProperties{
			Name:        "other",
			Devargs:     "0000:ff:00.0",
			Description: "something-else",
		})
		require.NoError(t, err)
		assert.False(t, isUnderlay)
	})

	t.Run("empty interface name in description returns error", func(t *testing.T) {
		_, _, err := groutPortToUnderlayInterface(&groutInterfaceProperties{
			Name:        "pci-port",
			Devargs:     "0000:ff:00.0",
			Description: UnderlayInterfaceDescriptionPrefix,
		})
		require.ErrorContains(t, err, "has no interface name")
	})
}

func TestUnderlayInterfacesToRemove(t *testing.T) {
	port := hostnetwork.UnderlayInterface{
		InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{PortName: new("u_eth0")},
	}
	withOptions := hostnetwork.UnderlayInterface{InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{}}
	noOptions := hostnetwork.UnderlayInterface{InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev}
	existing := []hostnetwork.UnderlayInterface{port}

	assert.Empty(t, underlayInterfacesToRemove(existing, []hostnetwork.UnderlayInterface{withOptions}))
	assert.Empty(t, underlayInterfacesToRemove(existing, []hostnetwork.UnderlayInterface{noOptions}))
	assert.Equal(t, existing, underlayInterfacesToRemove(existing, nil))

	customPort := port
	customPort.AcceleratedConfig = &hostnetwork.AcceleratedConfigParams{PortName: new("my-port")}
	withOptions.AcceleratedConfig.PortName = new("my-port")
	assert.Empty(t, underlayInterfacesToRemove([]hostnetwork.UnderlayInterface{customPort},
		[]hostnetwork.UnderlayInterface{withOptions}))
	withOptions.AcceleratedConfig.PortName = new("renamed")
	assert.Equal(t, []hostnetwork.UnderlayInterface{customPort},
		underlayInterfacesToRemove([]hostnetwork.UnderlayInterface{customPort}, []hostnetwork.UnderlayInterface{withOptions}))
}

func TestDeviceStateToUnderlayInterface(t *testing.T) {
	t.Run("custom port name is kept", func(t *testing.T) {
		got := deviceStateToUnderlayInterface(devicestate.Entry{InterfaceName: "ens1f0", PortName: "p0"})
		assert.Equal(t, "ens1f0", got.InterfaceName)
		assert.Equal(t, hostnetwork.UnderlayInterfaceNetDev, got.Kind)
		assert.Equal(t, "p0", PortName(got))

		requested := hostnetwork.UnderlayInterface{InterfaceName: "ens1f0", Kind: hostnetwork.UnderlayInterfaceNetDev,
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{PortName: new("p0")}}
		assert.Empty(t, underlayInterfacesToRemove([]hostnetwork.UnderlayInterface{got},
			[]hostnetwork.UnderlayInterface{requested}))
	})

	t.Run("state without port name uses the default", func(t *testing.T) {
		got := deviceStateToUnderlayInterface(devicestate.Entry{InterfaceName: "ens1f0"})
		assert.Nil(t, got.AcceleratedConfig)
		assert.Equal(t, "u_ens1f0", PortName(got))
	})
}

func TestMergeUnderlayInterfaces(t *testing.T) {
	fromGrout := hostnetwork.UnderlayInterface{InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{PortName: new("p0")}}
	fromState := hostnetwork.UnderlayInterface{InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev}
	stateOnly := hostnetwork.UnderlayInterface{InterfaceName: "eth1", Kind: hostnetwork.UnderlayInterfaceNetDev}
	hostOnly := hostnetwork.UnderlayInterface{InterfaceName: "eth2", Kind: hostnetwork.UnderlayInterfaceNetDev}
	fromHost := hostnetwork.UnderlayInterface{InterfaceName: "eth0", Kind: hostnetwork.UnderlayInterfaceNetDev}

	got := mergeUnderlayInterfaces(
		[]hostnetwork.UnderlayInterface{fromGrout},
		[]hostnetwork.UnderlayInterface{fromState, stateOnly},
		[]hostnetwork.UnderlayInterface{fromHost, hostOnly},
	)
	assert.Equal(t, []hostnetwork.UnderlayInterface{fromGrout, stateOnly, hostOnly}, got)
	assert.Empty(t, mergeUnderlayInterfaces())
}

func TestUnderlayPortOptions(t *testing.T) {
	queues, size := int32(4), int32(1024)
	iface := hostnetwork.UnderlayInterface{InterfaceName: "eth0", AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
		RXQueues: &queues, QSize: &size,
	}}
	assert.Equal(t, PortOptions{Description: "underlay-for=eth0", RXQueues: &queues, QSize: &size},
		underlayPortOptions(iface))
	assert.Equal(t, PortOptions{Description: "underlay-for=eth0"},
		underlayPortOptions(hostnetwork.UnderlayInterface{InterfaceName: "eth0"}))
}
