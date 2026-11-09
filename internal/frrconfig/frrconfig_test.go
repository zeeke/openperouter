// SPDX-License-Identifier:Apache-2.0

package frrconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openperouter/openperouter/internal/dockertest"
	"github.com/openperouter/openperouter/internal/frr"
	"github.com/openperouter/openperouter/internal/networklayerprotocol"
)

var tests = map[string]struct {
	failValidate bool
	failReload   bool
}{
	"/tmp/shouldPass": {
		failValidate: false,
		failReload:   false,
	},
	"/tmp/failValidate": {
		failValidate: true,
		failReload:   false,
	},
	"/tmp/failReload": {
		failValidate: false,
		failReload:   true,
	},
}

// asmMismatchPath is the fake config path whose reloader output mimics bgpd
// rejecting a reload because the running default BGP instance has a different
// AS than the desired configuration.
const asmMismatchPath = "/tmp/failASMismatch"

// showDaemons is the vtysh command used to poll which daemons answer.
const showDaemons = "show daemons"

// showRunningConfig is the vtysh command that dumps the running config.
const showRunningConfig = "show running-config"

const asmMismatchOutput = `Failed to execute no router bgp 64514
BGP instance name and AS number mismatch
BGP instance is already running; AS is 64514
line 9: Failure to communicate[13] to bgpd, line: router bgp 64520
`

func TestReload(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.Command }()

	// update() reads the desired config from the path before reloading, so
	// the fake paths must exist.
	for path := range tests {
		if err := os.WriteFile(path, []byte("frr version 10\n"), 0600); err != nil {
			t.Fatalf("failed to create the fake config %s: %v", path, err)
		}
		defer func(path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("failed to remove the fake config %s: %v", path, err)
			}
		}(path)
	}
	if err := os.WriteFile(asmMismatchPath, []byte("frr version 10\n"), 0600); err != nil {
		t.Fatalf("failed to create the fake config %s: %v", asmMismatchPath, err)
	}
	defer func() {
		if err := os.Remove(asmMismatchPath); err != nil {
			t.Fatalf("failed to remove the fake config %s: %v", asmMismatchPath, err)
		}
	}()

	for tc, params := range tests {
		t.Run(fmt.Sprintf("reload %s", tc), func(t *testing.T) {
			err := Update(tc)
			if (params.failReload || params.failValidate) && err == nil {
				t.Fatalf("expecting failure, got no error")
			}
			if params.failReload && !strings.Contains(err.Error(), "reload") {
				t.Fatalf("expecting reload error, got %v", err)
			}
			if params.failValidate && !strings.Contains(err.Error(), "test") {
				t.Fatalf("expecting test error, got %v", err)
			}
			if !params.failReload && !params.failValidate && err != nil {
				t.Fatalf("expecting no error, got %v", err)
			}
		})
	}
}

// TestReloadASMismatch exercises the recovery path end to end through the
// exec seam: the reloader fails with the bgpd AS mismatch output, the
// running BGP instances are cleared through vtysh, and the reload is
// attempted again. The fake reloader keeps failing, so the recovery
// ultimately errors out.
func TestReloadASMismatch(t *testing.T) {
	execCommand = fakeExecCommand
	defer func() { execCommand = exec.Command }()

	if err := os.WriteFile(asmMismatchPath, []byte("router bgp 64520\n"), 0600); err != nil {
		t.Fatalf("failed to create the fake config %s: %v", asmMismatchPath, err)
	}
	defer func() {
		if err := os.Remove(asmMismatchPath); err != nil {
			t.Fatalf("failed to remove the fake config %s: %v", asmMismatchPath, err)
		}
	}()

	err := Update(asmMismatchPath)
	if err == nil {
		t.Fatalf("expecting failure, got no error")
	}
	if !errors.Is(err, errASMismatch) {
		t.Fatalf("expecting an AS mismatch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed to reload after clearing the BGP instances") {
		t.Fatalf("expecting the post teardown reload to fail, got %v", err)
	}
}

// TestUpdateASMismatchRecovery asserts the recovery sequence for a reload
// that fails because the running bgpd holds a different AS than the desired
// config: the running BGP instances are cleared through separate vtysh
// sessions - VRF instances first - and the reload is attempted again, with
// one retry for a reload that fails right after the instance churn.
func TestUpdateASMismatchRecovery(t *testing.T) {
	tests := []struct {
		name      string
		failPhase string
		// failFirstRecoveryReload makes the first reload after the
		// teardown fail once, mimicking the frr-reload `vtysh write`
		// race the recovery retries.
		failFirstRecoveryReload bool
	}{
		{name: "mismatch on the test phase", failPhase: test},
		{name: "mismatch on the reload phase", failPhase: reload},
		{name: "recovery reload fails once", failPhase: reload, failFirstRecoveryReload: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "frr.conf")
			if err := os.WriteFile(path, []byte("router bgp 64520\n"), 0600); err != nil {
				t.Fatalf("failed to write the desired config: %v", err)
			}

			// runningConfig mimics a running bgpd with the old AS and a
			// dependent VRF instance, the state that wedges live reloads.
			const runningConfig = `frr version 10
router bgp 64512
exit
!
router bgp 64512 vrf red
exit
!
end
`
			var calls []string
			mismatchFailed := false
			recoveryReloadFailed := false
			reloadAction := func(_ string, action string) error {
				calls = append(calls, "reload "+action)
				if action == tt.failPhase && !mismatchFailed {
					mismatchFailed = true
					return fmt.Errorf("frr update %s failed: %w", action, errASMismatch)
				}
				// The write race hits the end of a reload that applied.
				if mismatchFailed && action == reload && tt.failFirstRecoveryReload && !recoveryReloadFailed {
					recoveryReloadFailed = true
					return errors.New("frr update reload failed: vtysh write race")
				}
				return nil
			}

			vtyshRunner := func(commands ...string) (string, error) {
				calls = append(calls, "vtysh "+strings.Join(commands, " "))
				switch strings.Join(commands, " ") {
				case showDaemons:
					return "zebra\nbgpd\n", nil
				case showRunningConfig:
					return runningConfig, nil
				}
				return "", nil
			}

			if err := update(path, reloadAction, vtyshRunner); err != nil {
				t.Fatalf("expecting the recovery to succeed, got %v", err)
			}

			// The desired config drives bgpd, so the update starts by
			// waiting for it. On the AS mismatch the running instances are
			// cleared - the VRF instance first, then the default one, each
			// through its own vtysh session - and the reload runs again.
			wantCalls := []string{
				"vtysh " + showDaemons,
				"vtysh show running-config",
			}
			if tt.failPhase == reload {
				wantCalls = append(wantCalls, "reload "+test)
			}
			wantCalls = append(wantCalls,
				"reload "+tt.failPhase,
				"vtysh show running-config",
				"vtysh configure terminal no router bgp 64512 vrf red end",
				"vtysh configure terminal no router bgp 64512 end",
			)
			if tt.failFirstRecoveryReload {
				wantCalls = append(wantCalls, "reload "+test, "reload "+reload)
			}
			wantCalls = append(wantCalls, "reload "+test, "reload "+reload)
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("unexpected call sequence:\ngot:  %v\nwant: %v", calls, wantCalls)
			}
		})
	}
}

// TestUpdateNoTeardownOnUnrelatedFailures asserts that reload failures
// which are not the bgpd AS mismatch class never clear the running BGP
// instances.
func TestUpdateNoTeardownOnUnrelatedFailures(t *testing.T) {
	tests := []struct {
		name      string
		failPhase string
	}{
		{name: "failure on the test phase", failPhase: test},
		{name: "failure on the reload phase", failPhase: reload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "frr.conf")
			if err := os.WriteFile(path, []byte("router bgp 64520\n"), 0600); err != nil {
				t.Fatalf("failed to write the desired config: %v", err)
			}

			reloadAction := func(_ string, action string) error {
				if action == tt.failPhase {
					return fmt.Errorf("frr update %s failed: some unrelated failure", action)
				}
				return nil
			}
			vtyshRunner := func(commands ...string) (string, error) {
				joined := strings.Join(commands, " ")
				// The desired config drives bgpd, so the update starts by
				// waiting for it to answer.
				if joined == showDaemons {
					return "zebra\nbgpd\n", nil
				}
				if strings.Contains(joined, "no router bgp") {
					t.Errorf("the running BGP instances must not be cleared for failures unrelated to AS changes, got %q", joined)
				}
				return "", nil
			}

			err := update(path, reloadAction, vtyshRunner)
			if err == nil {
				t.Fatal("expecting failure, got no error")
			}
			if !strings.Contains(err.Error(), "some unrelated failure") {
				t.Fatalf("expecting the original failure to be returned, got %v", err)
			}
		})
	}
}

func TestIsASMismatchFailure(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "instance already running with a different AS",
			output: "line 9: Failure to communicate[13] to bgpd, line: router bgp 64520\nBGP instance is already running; AS is 64514\n",
			want:   true,
		},
		{
			name:   "instance name and AS number mismatch",
			output: "BGP instance name and AS number mismatch\n",
			want:   true,
		},
		{
			name:   "cannot delete the default instance",
			output: "% Cannot delete default BGP instance. Dependent VRF instances exist\n",
			want:   false,
		},
		{
			name:   "generic reload failure",
			output: "% Configuration failed to apply\n",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isASMismatchFailure(tt.output); got != tt.want {
				t.Fatalf("isASMismatchFailure(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// TestBGPInstanceTeardownCommands asserts which `no router bgp` commands
// clear the BGP instances of a running config, VRF instances first.
func TestBGPInstanceTeardownCommands(t *testing.T) {
	tests := []struct {
		name          string
		runningConfig string
		want          []string
	}{
		{
			name: "default instance only",
			runningConfig: `frr version 10
router bgp 64512
exit
end
`,
			want: []string{"no router bgp 64512"},
		},
		{
			name: "default instance with a dependent VRF instance",
			runningConfig: `frr version 10
router bgp 64512
exit
!
router bgp 64512 vrf red
exit
end
`,
			want: []string{"no router bgp 64512 vrf red", "no router bgp 64512"},
		},
		{
			name: "instances with different ASNs",
			runningConfig: `router bgp 64512
exit
!
router bgp 64512 vrf red
exit
!
router bgp 65000 vrf blue
exit
end
`,
			want: []string{"no router bgp 64512 vrf red", "no router bgp 65000 vrf blue", "no router bgp 64512"},
		},
		{
			name:          "no BGP instances",
			runningConfig: "frr version 10\nend\n",
			want:          nil,
		},
		{
			name: "instance lines inside other contexts are ignored",
			runningConfig: `ip route 1.2.3.4/32 192.168.1.1
route-map allowall permit 1
 ! comment mentioning router bgp 999
end
`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bgpInstanceTeardownCommands(tt.runningConfig)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("bgpInstanceTeardownCommands() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTeardownBGPInstances asserts that each BGP instance is cleared through
// its own vtysh session and that a running config without BGP instances is a
// no-op.
func TestTeardownBGPInstances(t *testing.T) {
	t.Run("no BGP instances", func(t *testing.T) {
		var commands [][]string
		vtyshRunner := func(cmds ...string) (string, error) {
			commands = append(commands, cmds)
			if strings.Join(cmds, " ") == showRunningConfig {
				return "frr version 10\nend\n", nil
			}
			return "", nil
		}
		if err := teardownBGPInstances(vtyshRunner); err != nil {
			t.Fatalf("expecting no error, got %v", err)
		}
		want := [][]string{{showRunningConfig}}
		if !reflect.DeepEqual(commands, want) {
			t.Fatalf("unexpected vtysh calls:\ngot:  %v\nwant: %v", commands, want)
		}
	})

	t.Run("clears every instance, VRFs first", func(t *testing.T) {
		var commands [][]string
		vtyshRunner := func(cmds ...string) (string, error) {
			commands = append(commands, cmds)
			if strings.Join(cmds, " ") == showRunningConfig {
				return "router bgp 64512\nrouter bgp 64512 vrf red\nend\n", nil
			}
			return "", nil
		}
		if err := teardownBGPInstances(vtyshRunner); err != nil {
			t.Fatalf("expecting no error, got %v", err)
		}
		want := [][]string{
			{showRunningConfig},
			{"configure terminal", "no router bgp 64512 vrf red", "end"},
			{"configure terminal", "no router bgp 64512", "end"},
		}
		if !reflect.DeepEqual(commands, want) {
			t.Fatalf("unexpected vtysh calls:\ngot:  %v\nwant: %v", commands, want)
		}
	})

	t.Run("vtysh failure is returned", func(t *testing.T) {
		vtyshRunner := func(cmds ...string) (string, error) {
			if strings.Join(cmds, " ") == showRunningConfig {
				return "router bgp 64512\nend\n", nil
			}
			return "", errors.New("vtysh failed")
		}
		err := teardownBGPInstances(vtyshRunner)
		if err == nil {
			t.Fatal("expecting an error, got none")
		}
		if !strings.Contains(err.Error(), "failed to clear the BGP instance") {
			t.Fatalf("expecting a teardown error, got %v", err)
		}
	})
}

func TestWaitForBGPD(t *testing.T) {
	tests := []struct {
		name    string
		daemons []string
		wantErr bool
	}{
		{
			name:    "bgpd is up immediately",
			daemons: []string{"zebra\nbgpd\n"},
		},
		{
			name:    "bgpd comes back after a few polls",
			daemons: []string{"zebra\n", "zebra\n", "zebra\nbgpd\n"},
		},
		{
			name:    "bgpd never comes back",
			daemons: []string{"zebra\n"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			poll := 0
			vtyshRunner := func(commands ...string) (string, error) {
				output := tt.daemons[min(poll, len(tt.daemons)-1)]
				poll++
				return output, nil
			}

			err := waitForBGPD(vtyshRunner, 100*time.Millisecond, 5*time.Millisecond)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expecting a timeout error, got none")
				}
				if !strings.Contains(err.Error(), "bgpd did not answer") {
					t.Fatalf("expecting a timeout error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expecting no error, got %v", err)
			}
		})
	}
}

// This is not a real test. It's used in case fakeExecCommand is used in place of exec.Command.
// In that case the command execution is redirected to this function.
func TestFakeReloadHelper(t *testing.T) {
	if os.Getenv("WANT_FAKE_PYTHON") != "true" {
		return
	}

	args := os.Args

	for len(args) > 0 {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}

	// vtysh calls are invoked as "-c <command>".
	if len(args) > 0 && args[0] == "-c" {
		if len(args) > 1 && args[1] == showDaemons {
			fmt.Print("zebra\nbgpd\n")
		}
		if len(args) > 1 && args[1] == showRunningConfig {
			// A running bgpd with the old AS and a dependent VRF instance,
			// so the AS mismatch recovery clears them. No ISIS, so the
			// stale-ISIS teardown short-circuits.
			fmt.Print("router bgp 64512\nrouter bgp 64512 vrf red\n")
		}
		os.Exit(0)
	}

	if len(args) != 5 {
		fmt.Printf("expecting 5 args, got %v", args)
		os.Exit(1)
	}

	if !reflect.DeepEqual(args[0], reloaderPath) {
		fmt.Println("expected to be called with -c reloader args", args)
		os.Exit(1)
	}
	action, _ := strings.CutPrefix(args[1], "--")
	path := args[4]

	if path == asmMismatchPath {
		fmt.Print(asmMismatchOutput)
		os.Exit(1)
	}

	params, ok := tests[path]
	if !ok {
		fmt.Println("received untestable path", path)
		os.Exit(1)
	}
	if params.failReload && action == reload {
		fmt.Println("reload failed")
		os.Exit(1)
	}
	if params.failValidate && action == test {
		fmt.Println("test failed")
		os.Exit(1)
	}

	os.Exit(0)
}

// TestUpdate tests the Update() function by running a full configuration write and reload against an openperouter
// FRR docker container. This test allows us to unit test transitions from and to specific FRR configurations and thus
// to catch any issues with the frr-reload.py script before these issues haunt us in the E2E lanes. The added benefit
// of testing the frrconfig.Update() function is that we can take corrective action inside Update() to address any bugs
// caused by the frr-reload.py script.
func TestUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping FRR container integration test")
	}

	tcs := []struct {
		name string
		// wantRunningContains, when not empty, is a snippet expected in the
		// running config after applying `after`.
		wantRunningContains string
		before              frr.Config
		after               frr.Config
	}{
		{
			name:   "TestBasic",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
					},
				},
			},
		},
		{
			name:   "TestBasicWithASNRT",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
						ExportRTs: []string{"65000:1000"},
						ImportRTs: []string{"65000:1000"},
					},
				},
			},
		},
		{
			name:   "TestBasicWithIPRT",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
						ExportRTs: []string{"10.0.0.1:1000"},
						ImportRTs: []string{"10.0.0.1:1000"},
					},
				},
			},
		},
		{
			name:   "TestExternal",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromType("External"),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromType("External"),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
					},
				},
			},
		},
		{
			name:   "TestInternal",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromType("Internal"),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64512),
							Addr: "192.168.1.3",
							ID:   "192.168.1.3",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
					},
				},
			},
		},
		{
			name:   "TestDualStack",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
						ToAdvertiseIPv6: []string{
							"2001:db8::2/64",
						},
					},
				},
			},
		},
		{
			name:   "TestDualStackWithRT",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
						ToAdvertiseIPv6: []string{
							"2001:db8::2/64",
						},
						ExportRTs: []string{"65000:1000", "10.0.0.1:1000"},
						ImportRTs: []string{"65000:1000", "10.0.0.1:1000"},
					},
				},
			},
		},
		{
			name:   "TestIPv6Only",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
							ExtendedNexthop: true,
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
						},
						ToAdvertiseIPv6: []string{
							"2001:db8::2/64",
						},
					},
				},
			},
		},
		{
			name:   "TestBGPUnnumbered",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:       mustNewPeerASNFromNumber(64512),
							Interface: "eth1",
							ID:        "eth1",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
							ExtendedNexthop: true,
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64512),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
						},
						ToAdvertiseIPv6: []string{
							"2001:db8::2/64",
						},
					},
				},
			},
		},
		{
			name:   "BGPUnnumberedEBGPIPv6",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:       mustNewPeerASNFromNumber(64513),
							Interface: "eth1",
							ID:        "eth1",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
							ExtendedNexthop: true,
						},
					},
				},
			},
		},
		{
			name:   "IPv6OnlyWithRT",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
						},
						ToAdvertiseIPv6: []string{
							"2001:db8::2/64",
						},
						ExportRTs: []string{"65000:1000"},
						ImportRTs: []string{"65000:1000"},
					},
				},
			},
		},
		{
			name:   "NoL3VNIs",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
							},
						},
					},
				},
			},
		},
		{
			name:   "BFDEnabled",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
							},
							BFDEnabled: true,
						},
					},
				},
			},
		},
		{
			name:   "BFDProfile",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 64512,
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
							},
							BFDEnabled: true,
							BFDProfile: "foo",
						},
					},
				},
				BFDProfiles: []frr.BFDProfile{
					{
						Name:            "foo",
						ReceiveInterval: new(int32(43)),
					},
				},
			},
		},
		{
			name:   "L3VNIWithoutLocalNeighborAndAdvertise",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						RouterID: "10.0.0.1",
						VRF:      "red",
						VNI:      100,
						ASN:      64512,
					},
				},
			},
		},
		{
			name:   "L3VNIWithLocalNeighborAndRedistributeConnected",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
					},
				},
			},
		},
		{
			name:   "PassthroughNoEVPN",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				Passthrough: &frr.PassthroughConfig{
					LocalNeighborV4: &frr.NeighborConfig{
						ASN:         mustNewPeerASNFromNumber(64513),
						Addr:        "192.168.1.3",
						ID:          "192.168.1.3",
						ConnectTime: new(int64(5)),
					},
					ToAdvertiseIPv4: []string{
						"192.169.20.0/24",
						"192.169.21.0/24",
					},
				},
			},
		},
		{
			name:   "PassthroughExternal",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				Passthrough: &frr.PassthroughConfig{
					LocalNeighborV4: &frr.NeighborConfig{
						ASN:         mustNewPeerASNFromType("External"),
						Addr:        "192.168.1.3",
						ID:          "192.168.1.3",
						ConnectTime: new(int64(5)),
					},
					ToAdvertiseIPv4: []string{
						"192.169.20.0/24",
						"192.169.21.0/24",
					},
				},
			},
		},
		{
			name:   "PassthroughV4",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				Passthrough: &frr.PassthroughConfig{
					LocalNeighborV4: &frr.NeighborConfig{
						ASN:         mustNewPeerASNFromNumber(64513),
						Addr:        "192.168.1.3",
						ID:          "192.168.1.3",
						ConnectTime: new(int64(5)),
					},
					ToAdvertiseIPv4: []string{
						"192.169.20.0/24",
						"192.169.21.0/24",
					},
				},
			},
		},
		{
			name:   "PassthroughDual",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{ // Override to only IPv4 (auto-detection would be dualstack).
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
							},
						},
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "2001:db8::1",
							ID:   "2001:db8::1",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
							},
						},
					},
				},
				Passthrough: &frr.PassthroughConfig{
					LocalNeighborV4: &frr.NeighborConfig{
						ASN:         mustNewPeerASNFromNumber(64513),
						Addr:        "192.168.1.3",
						ID:          "192.168.1.3",
						ConnectTime: new(int64(5)),
					},
					LocalNeighborV6: &frr.NeighborConfig{
						ASN:         mustNewPeerASNFromNumber(64513),
						Addr:        "2001:db8:20::2",
						ID:          "2001:db8:20::2",
						ConnectTime: new(int64(5)),
					},
					ToAdvertiseIPv4: []string{
						"192.169.20.0/24",
						"192.169.21.0/24",
					},
					ToAdvertiseIPv6: []string{
						"2001:db8:20::/64",
						"2001:db8:21::/64",
					},
				},
				L3VNIs: []frr.L3VNIConfig{},
			},
		},
		{
			name:   "RawConfig",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				RawConfig: []frr.RawFRRSnippet{
					{Priority: new(int32(5)), Config: "ip prefix-list raw-low seq 10 permit 10.0.0.0/8"},
					{Priority: new(int32(20)), Config: "ip prefix-list raw-high seq 10 permit 10.1.0.0/16"},
				},
			},
		},
		{
			name:   "TunnelEndpointConfig",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "192.168.10.1/24",
						IPv6CIDR: "2001:db8:192:168::1/64",
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
						},
						ToAdvertiseIPv4: []string{
							"192.169.10.2/24",
						},
					},
				},
			},
		},
		{
			name: "SRv6 configuration (test segment-routing standalone, incomplete config)",
			before: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64512),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
			},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64512),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
					SegmentRouting: &frr.UnderlaySegmentRouting{
						SourceAddress: "2001:db8:1::1",
						Locator: frr.SRV6Locator{
							Name:     "Main",
							Prefix:   "2001:db8::/48",
							BlockLen: 32,
							NodeLen:  16,
							Behavior: "usid",
							Format:   "usid-f3216",
						},
						EncapBehavior: frr.HEncapsRed,
					},
				},
			},
		},
		{
			name:   "TestL2VNIWithRouteTargets",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "100.64.0.1/32",
					},
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "192.168.1.2",
							ID:   "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
						},
					},
				},
				L2VNIs: []frr.L2VNIConfig{
					{
						VNI:       100,
						ExportRTs: []string{"65000:100", "192.0.2.1:100"},
						ImportRTs: []string{"65001:100"},
					},
				},
			},
		},
		{
			name: "ISIS standalone configuration",
			before: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64512),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
			},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64512),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
					ISIS: &frr.UnderlayISIS{
						Net:   frr.MustParseISISNet("49.0001.0002.0003.0004.00"),
						Name:  "ISIS",
						Level: 1,
						Interfaces: []frr.ISISInterface{
							{Name: "vtep-lo", IPv6: true, IsPassive: true},
							{Name: "eth0", IPv4: true, IPv6: false},
							{Name: "eth1", IPv4: false, IPv6: true},
							{Name: "eth2", IPv4: true, IPv6: true},
						},
					},
				},
			},
		},
		{
			name:   "SegmentRoutingWithL2VNI",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN: 65000,
					ISIS: &frr.UnderlayISIS{
						Name:  "ISIS",
						Net:   frr.MustParseISISNet("49.0001.0002.0003.0004.00"),
						Level: 1,
						Interfaces: []frr.ISISInterface{
							{Name: "vtep-lo", IPv6: true, IsPassive: true},
							{Name: "eth0", IPv4: true, IPv6: true},
						},
					},
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							Name: "65001@192.168.122.1",
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "192.168.122.1",
							ID:   "192.168.122.1",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
							},
							EBGPMultiHop:    false,
							ExtendedNexthop: false,
						},
						{
							Name: "65001@2001:db8:192:168:1::1",
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "2001:db8:192:168:1::1",
							ID:   "2001:db8:192:168:1::1",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.Unicast},
								{AFI: networklayerprotocol.L2VPN, SAFI: networklayerprotocol.EVPN},
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.VPN},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.VPN},
							},
							EBGPMultiHop:    false,
							ExtendedNexthop: true,
							UpdateSource:    "2001:db8:1234:5678::",
						},
					},
					TunnelEndpoint: &frr.TunnelEndpoint{
						IPv4CIDR: "192.168.123.0/32",
						IPv6CIDR: "2001:db8:1234:5678::/128",
					},
					SegmentRouting: &frr.UnderlaySegmentRouting{
						SourceAddress: "2001:db8:1234:5678::",
						Locator: frr.SRV6Locator{
							Name:     "MAIN",
							Prefix:   "fd00:0:32::/48",
							BlockLen: 32,
							NodeLen:  16,
							Behavior: "usid",
							Format:   "usid-f3216",
						},
						EncapBehavior: frr.HEncapsRed,
					},
				},
				VPNs: []frr.L3VPNConfig{
					{
						ASN:             65000,
						ToAdvertiseIPv4: []string{"192.168.2.2/32"},
						ToAdvertiseIPv6: []string{},
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "192.168.2.2",
							ID:   "192.168.2.2",
						},
						VRF:                "vrf1",
						ExportRTs:          []string{"65000:100 11110:100"},
						ImportRTs:          []string{"65001:100 11111:100"},
						RouteDistinguisher: "10.0.0.1:100",
						RouterID:           "10.0.0.1",
					},
					{
						ASN:             65000,
						ToAdvertiseIPv4: []string{},
						ToAdvertiseIPv6: []string{"2001:db8::2/128"},
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
						},
						VRF:                "vrf1",
						ExportRTs:          []string{"65000:100 11110:100"},
						ImportRTs:          []string{"65001:100 11111:100"},
						RouteDistinguisher: "10.0.0.1:100",
						RouterID:           "10.0.0.1",
					},
				},
			},
		},
		{
			name:   "ISISAdvertisePassiveOnly",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64512),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
					ISIS: &frr.UnderlayISIS{
						Net:                  frr.MustParseISISNet("49.0001.0002.0003.0004.00"),
						Name:                 "ISIS",
						Level:                1,
						AdvertisePassiveOnly: true,
						Interfaces: []frr.ISISInterface{
							{Name: "vtep-lo", IPv6: true, IsPassive: true},
							{Name: "eth0", IPv4: true, IPv6: false},
							{Name: "eth1", IPv4: false, IPv6: true},
							{Name: "eth2", IPv4: true, IPv6: true},
						},
					},
				},
			},
		},
		{
			name: "UnderlayASNChange",
			before: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
			},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64520,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64520),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
			},
			wantRunningContains: "router bgp 64520",
		},
		{
			// bgpd cannot delete its default BGP instance while the VRF
			// instance of the L3VNI depends on it, so changing the underlay
			// ASN while keeping the L3VNI wedges live reloads. Update() must
			// recover by restarting bgpd with the desired config in place.
			name: "UnderlayASNChangeWithDependentVRF",
			before: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64513),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
					},
				},
			},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64520,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:                   mustNewPeerASNFromNumber(64520),
							Addr:                  "192.168.1.2",
							ID:                    "192.168.1.2",
							NetworkLayerProtocols: []networklayerprotocol.NLP{{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.Unicast}},
						},
					},
				},
				L3VNIs: []frr.L3VNIConfig{
					{
						VRF:      "red",
						ASN:      64512,
						VNI:      100,
						RouterID: "10.0.0.1",
					},
				},
			},
			wantRunningContains: "router bgp 64520",
		},
		{
			name:   "SegmentRouting full configuration",
			before: frr.Config{},
			after: frr.Config{
				Underlay: frr.UnderlayConfig{
					MyASN:    64512,
					RouterID: "10.0.0.1",
					Neighbors: []frr.NeighborConfig{
						{
							ASN:  mustNewPeerASNFromNumber(64513),
							Addr: "fc00::2:172:31:1:12",
							ID:   "fc00::2:172:31:1:12",
							NetworkLayerProtocols: []networklayerprotocol.NLP{
								{AFI: networklayerprotocol.IPv4, SAFI: networklayerprotocol.VPN},
								{AFI: networklayerprotocol.IPv6, SAFI: networklayerprotocol.VPN},
							},
							ExtendedNexthop: true,
							UpdateSource:    "fc00::2:172:31:1:32",
						},
					},
					ISIS: &frr.UnderlayISIS{
						Net:   frr.MustParseISISNet("49.0001.0002.0003.0004.00"),
						Name:  "ISIS",
						Level: 1,
						Interfaces: []frr.ISISInterface{
							{Name: "vtep-lo", IPv6: true, IsPassive: true},
							{Name: "eth0", IPv4: false, IPv6: true},
						},
					},
					SegmentRouting: &frr.UnderlaySegmentRouting{
						SourceAddress: "fc00::2:172:31:1:32",
						Locator: frr.SRV6Locator{
							Name:     "MAIN",
							Prefix:   "fd00:0:32::/48",
							BlockLen: 32,
							NodeLen:  16,
							Behavior: "usid",
							Format:   "usid-f3216",
						},
						EncapBehavior: frr.HEncaps,
					},
				},
				VPNs: []frr.L3VPNConfig{
					{
						ASN:             65000,
						ToAdvertiseIPv4: []string{"192.168.2.2/32"},
						ToAdvertiseIPv6: []string{},
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "192.168.2.2",
							ID:   "192.168.2.2",
						},
						VRF:                "vrf1",
						ExportRTs:          []string{"65000:100 65000:101"},
						ImportRTs:          []string{"65001:102 65001:103"},
						RouteDistinguisher: "10.0.0.1:100",
						RouterID:           "10.0.0.1",
					},
					{
						ASN:             65000,
						ToAdvertiseIPv4: []string{},
						ToAdvertiseIPv6: []string{"2001:db8::2/128"},
						LocalNeighbor: &frr.NeighborConfig{
							ASN:  mustNewPeerASNFromNumber(65001),
							Addr: "2001:db8::2",
							ID:   "2001:db8::2",
						},
						VRF:                "vrf2",
						ExportRTs:          []string{"65002:100 65002:101"},
						ImportRTs:          []string{"65003:102 65003:103"},
						RouteDistinguisher: "10.0.0.1:101",
						RouterID:           "10.0.0.1",
					},
				},
			},
		},
	}

	dir := t.TempDir()
	configFile := filepath.Join(dir, "frr.conf")

	updaterFn := func(_ context.Context, config string) error {
		if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
			return fmt.Errorf("failed to write the config to %s", configFile)
		}
		return nil
	}

	// waitForRunningConfig bounds how long assertRunningConfigContains waits
	// for the running config to converge before failing.
	const waitForRunningConfig = 90 * time.Second

	reloadFn := func(t *testing.T, config frr.Config) {
		t.Helper()
		if err := frr.ApplyConfig(context.TODO(), config, updaterFn); err != nil {
			t.Fatalf("Failed to apply config: %s", err)
		}
		// update() itself waits for bgpd before reloading: bgpd can crash
		// mid test (a known upstream FRR issue) and frr-reload reports
		// success without applying the BGP config while it is down.
		err := update(configFile, dockertest.FRRReload, dockertest.RunVtysh)
		if err != nil {
			t.Fatalf("Failed to update FRR with config: %s", err)
		}
	}

	// assertRunningConfigContains waits for the snippet to show up in the
	// running config, tolerating a bgpd crash mid test (a known upstream
	// FRR issue): watchfrr restarts bgpd from frr.conf - which holds the
	// desired config - within a few seconds, and the next reload
	// converges it.
	assertRunningConfigContains := func(t *testing.T, snippet string) {
		t.Helper()
		deadline := time.Now().Add(waitForRunningConfig)
		for {
			running, err := dockertest.RunVtysh(showRunningConfig)
			if err != nil {
				t.Fatalf("Failed to read the running config: %s", err)
			}
			if strings.Contains(running, snippet) {
				return
			}
			if !time.Now().Before(deadline) {
				daemons, _ := dockertest.RunVtysh("show daemons")
				watchfrr, _ := dockertest.RunVtysh("show watchfrr")
				t.Fatalf("Expecting the running config to contain %q within %s, got:\n%s\ndaemons: %s\nwatchfrr:\n%s",
					snippet, waitForRunningConfig, running, daemons, watchfrr)
			}
			time.Sleep(500 * time.Millisecond) //nolint:forbidigo // poll interval of the bounded wait
		}
	}

	dockertest.TestWithDockerT(t, func(t *testing.T) {
		for _, tc := range tcs {
			t.Run(fmt.Sprintf("add %s", tc.name), func(t *testing.T) {
				reloadFn(t, tc.before)
				reloadFn(t, tc.after)
				if tc.wantRunningContains != "" {
					assertRunningConfigContains(t, tc.wantRunningContains)
				}
			})
			t.Run(fmt.Sprintf("remove %s", tc.name), func(t *testing.T) {
				reloadFn(t, tc.after)
				reloadFn(t, tc.before)
			})
		}
	})
}

// helper function that redirects the execution to a mock process implemented by
// TestHelperProcess
func fakeExecCommand(name string, args ...string) *exec.Cmd {
	//nolint:prealloc
	cs := []string{"-test.run=TestFakeReloadHelper", "--"}
	cs = append(cs, args...)
	env := []string{
		"WANT_FAKE_PYTHON=true",
	}

	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = append(env, os.Environ()...)
	return cmd
}

func mustNewPeerASNFromNumber(number int64) frr.PeerASN {
	if number == 0 {
		panic("number must be > 0")
	}
	asn, err := frr.NewPeerASN(&number, nil)
	if err != nil {
		panic(err)
	}
	return asn
}

func mustNewPeerASNFromType(t string) frr.PeerASN {
	asn, err := frr.NewPeerASN(nil, &t)
	if err != nil {
		panic(err)
	}
	return asn
}
