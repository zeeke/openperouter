// SPDX-License-Identifier:Apache-2.0

package grout

import "testing"

func TestUnderlayDevargs(t *testing.T) {
	tests := []struct {
		name    string
		nicName string
		want    string
	}{
		{
			name:    "simple nic",
			nicName: "eth0",
			want:    "net_tap0,iface=gr-eth0,remote=eth0",
		},
		{
			name:    "bond interface",
			nicName: "bond0",
			want:    "net_tap0,iface=gr-bond0,remote=bond0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnderlayDevargs(tt.nicName)
			if got != tt.want {
				t.Errorf("UnderlayDevargs(%q) = %q, want %q", tt.nicName, got, tt.want)
			}
		})
	}
}

func TestPassthroughDevargs(t *testing.T) {
	got := PassthroughDevargs()
	want := "net_tap1,iface=gr-pt"
	if got != want {
		t.Errorf("PassthroughDevargs() = %q, want %q", got, want)
	}
}

func TestPassthroughTAPName(t *testing.T) {
	if PassthroughTAPName != "gr-pt" {
		t.Errorf("PassthroughTAPName = %q, want %q", PassthroughTAPName, "gr-pt")
	}
}

func TestNewClient(t *testing.T) {
	c := NewClient("/var/run/grout/grout.sock")
	if c.socketPath != "/var/run/grout/grout.sock" {
		t.Errorf("NewClient socket path = %q, want %q", c.socketPath, "/var/run/grout/grout.sock")
	}
}
