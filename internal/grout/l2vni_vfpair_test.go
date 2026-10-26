// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrunkVFPortName(t *testing.T) {
	const maxVLAN = 4094

	tests := []struct {
		name        string
		trunkVFName string
		want        string
	}{
		{
			name:        "long VF name",
			trunkVFName: "toleafkind1v1",
			want:        "t_jpjv40",
		},
		{
			name:        "VF name one character too long",
			trunkVFName: "enp1s0v12",
			want:        "t_wm3pa3",
		},
		{
			name:        "VF name at the maximum length",
			trunkVFName: "enp1s0v1",
			want:        "t_enp1s0v1",
		},
		{
			name:        "short VF name",
			trunkVFName: "a",
			want:        "t_a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trunkVFPortName(tt.trunkVFName)
			require.Equal(t, tt.want, got)
			vlanIfName := VLANSubInterfaceName(maxVLAN, got)
			require.Less(t, len(vlanIfName), syscall.IFNAMSIZ, "VLAN sub-interface name %s too long", vlanIfName)
		})
	}
}
