// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clientset "k8s.io/client-go/kubernetes"
)

// Trunk VF identifiers in the QEMU VM (2nd igb NIC, bound to grout via DPDK).
const (
	trunkVFPCI         = "0000:02:00.0"
	trunkVFNetlinkName = "enp2s0"
)

var _ = Describe("QEMU L2VNI VF-to-VF", Ordered, QEMUSupport, GroutSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod

	qemuUnderlay := v1alpha1.Underlay{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "underlay",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.UnderlaySpec{
			ASN: 64514,
			Interfaces: []v1alpha1.UnderlayInterface{
				{
					Type: v1alpha1.UnderlayInterfaceTypeNetworkDevice,
					NetworkDevice: &v1alpha1.NetworkDevice{
						InterfaceName:     "enp1s0",
						AcceleratedConfig: &v1alpha1.AcceleratedConfig{},
					},
				},
			},
			Neighbors: []v1alpha1.Neighbor{
				{
					ASN:     ptr.To(int64(65000)),
					Address: ptr.To("192.168.100.1"),
				},
			},
		},
	}

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
				HostASN: ptr.To(int64(64515)),
				LocalCIDR: v1alpha1.LocalCIDRConfig{
					IPv4: ptr.To("192.169.10.0/24"),
				},
			},
		},
	}

	l2vniA := v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-a",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L2VNISpec{
			VNI:           110,
			RoutingDomain: l3vniRoutingDomain("red"),
			GatewayIPs:    []string{"10.110.0.1/24"},
			SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
				PCIAddress: ptr.To(trunkVFPCI),
				VLAN:       500,
			},
		},
	}

	l2vniB := v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tenant-b",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L2VNISpec{
			VNI:           120,
			RoutingDomain: l3vniRoutingDomain("red"),
			GatewayIPs:    []string{"10.120.0.1/24"},
			SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
				PCIAddress: ptr.To(trunkVFPCI),
				VLAN:       600,
			},
		},
	}

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty(), "no router pods found")
		DumpPods("Router pods", routerPods)

		By("Creating accelerated underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuUnderlay},
		})).To(Succeed())

		By("Waiting for BGP session with TOR")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			validateSessionWithNeighbor(exec, validationParameters{
				fromName:    pod.Name,
				toName:      "qemu-tor",
				neighborIP:  "192.168.100.1",
				established: Established,
			})
		}
	})

	AfterAll(func() {
		if !QEMUMode {
			return
		}
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
		dumpIfFails(cs)
	})

	It("should create L2VNIs with VF-pair configuration", func() {
		By("Creating L3VNI red and L2VNIs with sriovVFPair")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
			L2VNIs: []v1alpha1.L2VNI{l2vniA, l2vniB},
		})).To(Succeed())
	})

	It("should have grout VLAN sub-interfaces for VF-pair L2VNIs", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				if err != nil {
					return fmt.Errorf("grcli interface show failed on %s: %s", pod.Name, out)
				}

				var ifaces []groutInterface
				if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
					return fmt.Errorf("failed to parse grcli output on %s: %w", pod.Name, err)
				}

				var hasVlan500, hasVlan600 bool
				for _, iface := range ifaces {
					if iface.Type == "vlan" {
						if containsVLANID(iface.Name, 500) {
							hasVlan500 = true
						}
						if containsVLANID(iface.Name, 600) {
							hasVlan600 = true
						}
					}
				}

				if !hasVlan500 {
					return fmt.Errorf("VLAN 500 sub-interface not found on %s, interfaces: %v", pod.Name, ifaces)
				}
				if !hasVlan600 {
					return fmt.Errorf("VLAN 600 sub-interface not found on %s, interfaces: %v", pod.Name, ifaces)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should have grout bridges for both L2VNIs", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				if err != nil {
					return fmt.Errorf("grcli interface show failed on %s: %s", pod.Name, out)
				}

				var ifaces []groutInterface
				if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
					return fmt.Errorf("failed to parse grcli output on %s: %w", pod.Name, err)
				}

				var hasBridge110, hasBridge120 bool
				for _, iface := range ifaces {
					if iface.Type == "bridge" {
						if iface.Name == "br-pe-110" {
							hasBridge110 = true
						}
						if iface.Name == "br-pe-120" {
							hasBridge120 = true
						}
					}
				}

				if !hasBridge110 {
					return fmt.Errorf("bridge br-pe-110 not found on %s", pod.Name)
				}
				if !hasBridge120 {
					return fmt.Errorf("bridge br-pe-120 not found on %s", pod.Name)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should have VXLAN interfaces for both VNIs", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				if err != nil {
					return fmt.Errorf("grcli interface show failed on %s: %s", pod.Name, out)
				}

				var ifaces []groutInterface
				if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
					return fmt.Errorf("failed to parse grcli output on %s: %w", pod.Name, err)
				}

				var hasVxlan110, hasVxlan120 bool
				for _, iface := range ifaces {
					if iface.Type == "vxlan" {
						if iface.Name == "vni110" {
							hasVxlan110 = true
						}
						if iface.Name == "vni120" {
							hasVxlan120 = true
						}
					}
				}

				if !hasVxlan110 {
					return fmt.Errorf("VXLAN vni110 not found on %s", pod.Name)
				}
				if !hasVxlan120 {
					return fmt.Errorf("VXLAN vni120 not found on %s", pod.Name)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should configure FRR with VRF red and EVPN for the L2VNIs", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return fmt.Errorf("failed to get FRR running config from %s: %w", pod.Name, err)
				}
				for _, check := range []string{"vrf red", "vni 110", "vni 120"} {
					if !strings.Contains(cfg, check) {
						return fmt.Errorf("FRR config on %s missing %q:\n%s", pod.Name, check, cfg)
					}
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should not have host-side bridges (no br-hs-* for VF-pair L2VNIs)", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			out, err := exec.Exec("ip", "link", "show", "type", "bridge")
			Expect(err).NotTo(HaveOccurred(), "ip link show failed on %s: %s", pod.Name, out)
			Expect(out).NotTo(ContainSubstring("br-hs-110"),
				"host bridge br-hs-110 should not exist with VF-pair mode on %s", pod.Name)
			Expect(out).NotTo(ContainSubstring("br-hs-120"),
				"host bridge br-hs-120 should not exist with VF-pair mode on %s", pod.Name)
		}
	})

	It("should accept L2VNI with netlinkName selector", func() {
		l2vniNetlink := v1alpha1.L2VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tenant-netlink",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L2VNISpec{
				VNI:           130,
				RoutingDomain: l3vniRoutingDomain("red"),
				GatewayIPs:    []string{"10.130.0.1/24"},
				SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
					NetlinkName: ptr.To(trunkVFNetlinkName),
					VLAN:        700,
				},
			},
		}

		By("Creating L2VNI with netlinkName VF selector")
		Expect(Updater.Update(config.Resources{
			L2VNIs: []v1alpha1.L2VNI{l2vniA, l2vniB, l2vniNetlink},
		})).To(Succeed())

		By("Verifying grout creates the VLAN 700 sub-interface")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				if err != nil {
					return fmt.Errorf("grcli interface show failed on %s: %s", pod.Name, out)
				}

				var ifaces []groutInterface
				if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
					return fmt.Errorf("failed to parse grcli output on %s: %w", pod.Name, err)
				}

				for _, iface := range ifaces {
					if iface.Type == "vlan" && containsVLANID(iface.Name, 700) {
						return nil
					}
				}
				return fmt.Errorf("VLAN 700 sub-interface not found on %s", pod.Name)
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})

func containsVLANID(name string, vlan int) bool {
	suffix := fmt.Sprintf(".%d", vlan)
	return strings.HasSuffix(name, suffix)
}
