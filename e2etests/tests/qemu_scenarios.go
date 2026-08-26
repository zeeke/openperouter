// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/ipfamily"
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var AcceleratedUnderlay = v1alpha1.Underlay{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "underlay",
		Namespace: openperouter.Namespace,
	},
	Spec: v1alpha1.UnderlaySpec{
		ASN: 64514,
		Interfaces: []v1alpha1.UnderlayInterface{
			{
				Type: "NetworkDevice",
				NetworkDevice: &v1alpha1.NetworkDevice{
					InterfaceName:     "toswitch1v0",
					AcceleratedConfig: &v1alpha1.AcceleratedConfig{},
				},
			},
		},
		Neighbors: []v1alpha1.Neighbor{
			{
				ASN:                  new(int64(64512)),
				Address:              new("192.168.11.2"),
				ConnectTimeSeconds:   new(int64(5)),
				KeepaliveTimeSeconds: new(int64(3)),
				HoldTimeSeconds:      new(int64(9)),
			},
			{
				ASN:                  new(int64(64513)),
				Address:              new("192.168.12.2"),
				ConnectTimeSeconds:   new(int64(5)),
				KeepaliveTimeSeconds: new(int64(3)),
				HoldTimeSeconds:      new(int64(9)),
			},
		},
		TunnelEndpoint: &v1alpha1.TunnelEndpointConfig{
			CIDRs: []string{"100.65.0.0/24"},
		},
	},
}

var AcceleratedUnderlaySRv6 = v1alpha1.Underlay{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "underlay",
		Namespace: openperouter.Namespace,
	},
	Spec: v1alpha1.UnderlaySpec{
		ASN: 64514,
		Interfaces: []v1alpha1.UnderlayInterface{
			{
				Type: "NetworkDevice",
				NetworkDevice: &v1alpha1.NetworkDevice{
					InterfaceName:     "toswitch1v0",
					AcceleratedConfig: &v1alpha1.AcceleratedConfig{},
				},
			},
		},
		Neighbors: []v1alpha1.Neighbor{
			{
				ASN:                  new(int64(64520)),
				Address:              new("2001:db8:1234::1"),
				Properties:           []v1alpha1.NeighborProperty{{Type: v1alpha1.NeighborPropertyEBGPMultiHop}},
				ConnectTimeSeconds:   new(int64(5)),
				KeepaliveTimeSeconds: new(int64(3)),
				HoldTimeSeconds:      new(int64(9)),
			},
		},
		RouterIDCIDR: new("10.0.0.0/24"),
		TunnelEndpoint: &v1alpha1.TunnelEndpointConfig{
			CIDRs: []string{
				"2001:db8:1234:5678::/64",
			},
		},
		ISIS: &v1alpha1.ISISConfig{
			BaseNet: "49.0001.0002.0003.0004.00",
			Level:   new(int32(1)),
			Interfaces: []v1alpha1.ISISInterface{
				{
					Name:     "u_toswitch1v0",
					IPFamily: new(v1alpha1.IPFamilyIPv6),
				},
			},
		},
		SRV6: &v1alpha1.SRV6Config{
			Locator: v1alpha1.SRV6Locator{
				BasePrefix: "fd00:0:32::/48",
				Format:     "usid-f3216",
			},
		},
	},
}

// --- EVPN accelerated scenarios ---

const testNamespace = "test-clab-l2vni"

var _ = Describe("Clab accelerated EVPN scenarios", Ordered, GroutSupport, func() {
	var cs clientset.Interface
	var routers openperouter.Routers
	var nodes []corev1.Node

	BeforeAll(func() {
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		var err error
		routers, err = openperouter.Get(cs, HostMode)
		Expect(err).NotTo(HaveOccurred())
		routers.Dump(GinkgoWriter)

		nodes, err = k8s.GetNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		By("Creating accelerated EVPN underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{AcceleratedUnderlay},
		})).To(Succeed())

		By("Verifying BGP sessions with leafkind1")
		leafExec := executor.ForContainer(infra.KindLeaf)
		for _, node := range nodes {
			neighborIP, err := infra.NeighborIP(infra.KindLeaf, node.Name)
			Expect(err).NotTo(HaveOccurred())
			validateSessionWithNeighbor(leafExec, validationParameters{
				fromName:    infra.KindLeaf,
				toName:      node.Name,
				neighborIP:  neighborIP,
				established: Established,
			})
		}
	})

	AfterAll(func() {
		Expect(Updater.CleanAll()).To(Succeed())
		Eventually(func() error {
			routers, err := openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs, testNamespace)
		Expect(Updater.CleanButUnderlay()).To(Succeed())
		Expect(infra.LeafAConfig.Reset()).To(Succeed())
		Expect(infra.LeafBConfig.Reset()).To(Succeed())
	})

	It("should configure L3Passthrough host session in FRR", func() {
		passthrough := v1alpha1.L3Passthrough{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "passthrough",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L3PassthroughSpec{
				HostSession: v1alpha1.HostSession{
					ASN:     64514,
					HostASN: new(int64(64515)),
					LocalCIDR: v1alpha1.LocalCIDRConfig{
						IPv4: new("192.169.10.0/24"),
					},
				},
			},
		}

		By("Creating L3Passthrough")
		Expect(Updater.Update(config.Resources{
			L3Passthrough: []v1alpha1.L3Passthrough{passthrough},
		})).To(Succeed())

		By("Verifying host session CIDR in FRR running config")
		for exec := range routers.GetExecutors() {
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return fmt.Errorf("failed to get FRR running config from %s: %w", exec.Name(), err)
				}
				if !strings.Contains(cfg, "192.169.10.") {
					return fmt.Errorf("FRR config on %s does not contain host session CIDR:\n%s", exec.Name(), cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should receive Type-5 routes via L3VNI", func() {
		l3vniRed := v1alpha1.L3VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L3VNISpec{
				VRF: "red",
				VNI: 100,
				HostSession: &v1alpha1.HostSession{
					ASN:     64514,
					HostASN: new(int64(64515)),
					LocalCIDR: v1alpha1.LocalCIDRConfig{
						IPv4: new("192.169.10.0/24"),
					},
				},
			},
		}

		By("Creating L3VNI red")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
		})).To(Succeed())

		By("Configuring leafA to advertise routes in VRF red")
		Expect(infra.LeafAConfig.ChangePrefixes(emptyPrefixes, leafAVRFRedPrefixes, emptyPrefixes)).To(Succeed())

		By("Verifying Type-5 routes received from leafA")
		for exec := range routers.GetExecutors() {
			waitForType5Route(exec, "192.168.20.0/24")
		}
	})

	It("should route between L2VNIs in the same L3VNI routing domain", func() {
		l3vniRed := v1alpha1.L3VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L3VNISpec{
				VRF: "red",
				VNI: 100,
				HostSession: &v1alpha1.HostSession{
					ASN:     64514,
					HostASN: new(int64(64515)),
					LocalCIDR: v1alpha1.LocalCIDRConfig{
						IPv4: new("192.169.10.0/24"),
					},
				},
			},
		}

		By("Setting redistribute connected on leaves")
		Expect(infra.LeafAConfig.RedistributeConnected()).To(Succeed())
		Expect(infra.LeafBConfig.RedistributeConnected()).To(Succeed())

		By("Creating L3VNI red")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
		})).To(Succeed())

		l2Red110 := v1alpha1.L2VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red110",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L2VNISpec{
				VNI:           110,
				GatewayIPs:    []string{"192.171.10.1/24"},
				RoutingDomain: l3vniRoutingDomain("red"),
				HostMaster: &v1alpha1.HostMaster{
					Type: v1alpha1.LinuxBridge,
					LinuxBridge: &v1alpha1.LinuxBridgeConfig{
						Lifecycle: v1alpha1.BridgeLifecycleManaged,
					},
				},
			},
		}
		l2Red120 := v1alpha1.L2VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red120",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L2VNISpec{
				VNI:           120,
				GatewayIPs:    []string{"192.171.20.1/24"},
				RoutingDomain: l3vniRoutingDomain("red"),
				HostMaster: &v1alpha1.HostMaster{
					Type: v1alpha1.LinuxBridge,
					LinuxBridge: &v1alpha1.LinuxBridgeConfig{
						Lifecycle: v1alpha1.BridgeLifecycleManaged,
					},
				},
			},
		}

		By("Creating L2VNIs with L3VNI routing domain")
		Expect(Updater.Update(config.Resources{
			L2VNIs: []v1alpha1.L2VNI{l2Red110, l2Red120},
		})).To(Succeed())

		_, err := k8s.CreateNamespace(cs, testNamespace)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err := k8s.DeleteNamespace(cs, testNamespace)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Creating NADs for both L2VNIs")
		nad110, err := k8s.CreateMacvlanNad("nad-110", testNamespace, "br-hs-110", []string{"192.171.10.1/24"})
		Expect(err).NotTo(HaveOccurred())
		nad120, err := k8s.CreateMacvlanNad("nad-120", testNamespace, "br-hs-120", []string{"192.171.20.1/24"})
		Expect(err).NotTo(HaveOccurred())

		By("Creating pods on different L2VNIs")
		pod110, err := k8s.CreateAgnhostPod(cs, "pod-vni110", testNamespace,
			k8s.WithNad(nad110.Name, testNamespace, []string{"192.171.10.2/24"}))
		Expect(err).NotTo(HaveOccurred())
		pod120, err := k8s.CreateAgnhostPod(cs, "pod-vni120", testNamespace,
			k8s.WithNad(nad120.Name, testNamespace, []string{"192.171.20.2/24"}))
		Expect(err).NotTo(HaveOccurred())

		By("Removing the default gateway via the primary interface")
		Expect(removeGatewayFromPod(pod110)).To(Succeed())
		Expect(removeGatewayFromPod(pod120)).To(Succeed())

		By("Checking inter-VNI reachability via VRF red")
		exec110 := executor.ForPod(pod110.Namespace, pod110.Name, "agnhost")
		exec120 := executor.ForPod(pod120.Namespace, pod120.Name, "agnhost")
		canPingFromPod(exec110, "192.171.20.2")
		canPingFromPod(exec120, "192.171.10.2")
	})
})

// --- SRv6 accelerated scenario ---

var _ = Describe("Clab accelerated L3VPN scenario", Ordered, GroutSupport, func() {
	var cs clientset.Interface
	var routers openperouter.Routers

	l3vpnRed := v1alpha1.L3VPN{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VPNSpec{
			VRF: "red",
			HostSession: &v1alpha1.HostSession{
				ASN:     64514,
				HostASN: new(int64(64515)),
				LocalCIDR: v1alpha1.LocalCIDRConfig{
					IPv4: new("192.169.10.0/24"),
				},
			},
			RDAssignedNumber: 100,
			ExportRTs: []v1alpha1.RouteTarget{
				"64514:100",
			},
			ImportRTs: []v1alpha1.RouteTarget{
				"64520:100",
			},
		},
	}

	BeforeAll(func() {
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		var err error
		routers, err = openperouter.Get(cs, HostMode)
		Expect(err).NotTo(HaveOccurred())
		routers.Dump(GinkgoWriter)

		By("Resetting SRv6 leaf configuration")
		Expect(infra.LeafSRV6Config.Reset()).To(Succeed())

		By("Resetting leafkind configurations")
		nodes, err := k8s.GetNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(infra.LeafKind1Config.UpdateConfig(nodes, infra.LeafKindConfiguration{})).To(Succeed())
		Expect(infra.LeafKind2Config.UpdateConfig(nodes, infra.LeafKindConfiguration{})).To(Succeed())

		By("Creating accelerated SRv6 underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{AcceleratedUnderlaySRv6},
		})).To(Succeed())
	})

	AfterAll(func() {
		Expect(Updater.CleanAll()).To(Succeed())
		Expect(infra.LeafSRV6Config.Reset()).To(Succeed())
		Eventually(func() error {
			routers, err := openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs)
		Expect(Updater.CleanButUnderlay()).To(Succeed())
		Expect(infra.LeafSRV6Config.Reset()).To(Succeed())
	})

	It("should receive L3VPN routes via SRv6", func() {
		By("Creating L3VPN red")
		Expect(Updater.Update(config.Resources{
			L3VPNs: []v1alpha1.L3VPN{l3vpnRed},
		})).To(Succeed())

		By("Verifying FRR running config contains VRF red with SRv6")
		for exec := range routers.GetExecutors() {
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return err
				}
				if !strings.Contains(cfg, "vrf red") {
					return fmt.Errorf("FRR config on %s does not contain vrf red:\n%s", exec.Name(), cfg)
				}
				if !strings.Contains(cfg, "sid vpn per-vrf") {
					return fmt.Errorf("FRR config on %s does not contain SRv6 SID config:\n%s", exec.Name(), cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}

		By("Configuring leafSRV6 to advertise routes in VRF red")
		Expect(infra.LeafSRV6Config.ChangePrefixes(emptyPrefixes, leafSRV6VRFRedPrefixes, emptyPrefixes)).To(Succeed())

		By("Verifying L3VPN routes received from leafSRV6")
		for exec := range routers.GetExecutors() {
			Eventually(func() error {
				l3vpnInfo, err := frr.L3VPNInfo(exec, ipfamily.IPv4)
				if err != nil {
					return fmt.Errorf("failed to get L3VPN info from %s: %w", exec.Name(), err)
				}
				if !l3vpnInfo.ContainsBGPRouteForL3VPN(
					"192.170.20.0/24",
					infra.LeafSRV6Config.RouterID,
					l3vpnRed.Spec.ImportRTs) {
					return fmt.Errorf("L3VPN route 192.170.20.0/24 not found on %s, l3vpn info: %v", exec.Name(), l3vpnInfo)
				}
				return nil
			}, 3*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})
