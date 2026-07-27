// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"context"
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
	clientset "k8s.io/client-go/kubernetes"
)

const (
	qemuTORIP   = "192.168.100.1"
	qemuTORIPv6 = "2001:db8:100::1"
	qemuTORASN  = 65000
	qemuVMASN   = 64514
)

func qemuEVPNUnderlay() v1alpha1.Underlay {
	return v1alpha1.Underlay{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "underlay",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.UnderlaySpec{
			ASN: qemuVMASN,
			Interfaces: []v1alpha1.UnderlayInterface{{
				Type: v1alpha1.UnderlayInterfaceTypeGroutPort,
				GroutPort: &v1alpha1.GroutPortConfig{
					PCIAddress: new("0000:01:00.0"),
					IPAM: v1alpha1.GroutPortIPAM{
						Addresses: []string{"192.168.100.10/24"},
					},
				},
			}},
			Neighbors: []v1alpha1.Neighbor{{
				ASN:     new(int64(qemuTORASN)),
				Address: new(qemuTORIP),
			}},
			TunnelEndpoint: &v1alpha1.TunnelEndpointConfig{
				CIDRs: []string{"100.65.0.0/24"},
			},
		},
	}
}

func qemuSRv6Underlay() v1alpha1.Underlay {
	return v1alpha1.Underlay{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "underlay",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.UnderlaySpec{
			ASN: qemuVMASN,
			Interfaces: []v1alpha1.UnderlayInterface{{
				Type: v1alpha1.UnderlayInterfaceTypeGroutPort,
				GroutPort: &v1alpha1.GroutPortConfig{
					PCIAddress: new("0000:01:00.0"),
					IPAM: v1alpha1.GroutPortIPAM{
						Addresses: []string{"192.168.100.10/24"},
					},
				},
			}},
			Neighbors: []v1alpha1.Neighbor{{
				ASN:     new(int64(qemuTORASN)),
				Address: new(qemuTORIPv6),
			}},
			TunnelEndpoint: &v1alpha1.TunnelEndpointConfig{
				CIDRs: []string{"2001:db8:1234:5678::/64"},
			},
			RouterIDCIDR: new("10.0.0.0/24"),
			ISIS: &v1alpha1.ISISConfig{
				BaseNet: "49.0001.0002.0003.0004.00",
				Level:   new(int32(1)),
				Interfaces: []v1alpha1.ISISInterface{{
					Name:     "u_p000001000",
					IPFamily: new(v1alpha1.IPFamilyIPv6),
				}},
			},
			SRV6: &v1alpha1.SRV6Config{
				Locator: v1alpha1.SRV6Locator{
					BasePrefix: "fd00:0:32::/48",
					Format:     "usid-f3216",
				},
			},
		},
	}
}

// --- EVPN scenarios ---

var _ = Describe("QEMU EVPN scenarios", Ordered, QEMUSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod

	l3vniRed := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VNISpec{
			VRF: "red",
			VNI: 100,
			HostSession: &v1alpha1.HostSession{
				ASN:     qemuVMASN,
				HostASN: new(int64(64515)),
				LocalCIDR: v1alpha1.LocalCIDRConfig{
					IPv4: new("192.169.10.0/24"),
				},
			},
		},
	}

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty())
		DumpPods("Router pods", routerPods)

		By("Creating EVPN underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuEVPNUnderlay()},
		})).To(Succeed())

		By("Verifying BGP session with TOR")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			validateSessionWithNeighbor(exec, validationParameters{
				fromName:    pod.Name,
				toName:      "qemu-tor",
				neighborIP:  qemuTORIP,
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

	It("should receive EVPN Type-5 routes via L3VNI", func() {
		By("Creating L3VNI red")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
		})).To(Succeed())

		By("Verifying Type-5 route for 192.168.20.0/24 is received")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			waitForType5Route(exec, "192.168.20.0/24")
		}
	})

	It("should create L2VNI with L3VNI routing domain", func() {
		l2vni := v1alpha1.L2VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red110",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L2VNISpec{
				VNI:           110,
				RoutingDomain: l3vniRoutingDomain("red"),
				HostMaster: &v1alpha1.HostMaster{
					Type:        v1alpha1.LinuxBridge,
					LinuxBridge: &v1alpha1.LinuxBridgeConfig{AutoCreate: new(true)},
				},
			},
		}
		Expect(Updater.Update(config.Resources{
			L2VNIs: []v1alpha1.L2VNI{l2vni},
		})).To(Succeed())

		By("Verifying VNI 110 is provisioned in EVPN with tenant VRF red")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				vniInfo, err := frr.EVPNVNIStatus(exec, 110)
				if err != nil {
					return fmt.Errorf("failed to query EVPN VNI 110 on %s: %v", pod.Name, err)
				}
				if vniInfo == nil {
					return fmt.Errorf("VNI 110 not yet provisioned on %s", pod.Name)
				}
				if vniInfo.TenantVrf != "red" {
					return fmt.Errorf("VNI 110 on %s has tenant VRF %q, expected %q", pod.Name, vniInfo.TenantVrf, "red")
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})

// --- SRv6 scenarios ---

var _ = Describe("QEMU SRv6 scenarios", Ordered, QEMUSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod

	l3vpnRed := v1alpha1.L3VPN{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VPNSpec{
			VRF: "red",
			HostSession: &v1alpha1.HostSession{
				ASN:     qemuVMASN,
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
				"65000:100",
			},
		},
	}

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty())
		DumpPods("Router pods", routerPods)

		By("Creating SRv6 underlay with ISIS")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuSRv6Underlay()},
		})).To(Succeed())

		By("Verifying BGP session with TOR over IPv6")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			validateSessionWithNeighbor(exec, validationParameters{
				fromName:    pod.Name,
				toName:      "qemu-tor",
				neighborIP:  qemuTORIPv6,
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

	It("should receive L3VPN routes via SRv6", func() {
		By("Creating L3VPN red")
		Expect(Updater.Update(config.Resources{
			L3VPNs: []v1alpha1.L3VPN{l3vpnRed},
		})).To(Succeed())

		By("Verifying FRR running config contains VRF red with SRv6")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return err
				}
				if !strings.Contains(cfg, "vrf red") {
					return fmt.Errorf("FRR config on %s does not contain vrf red:\n%s", pod.Name, cfg)
				}
				if !strings.Contains(cfg, "sid vpn per-vrf") {
					return fmt.Errorf("FRR config on %s does not contain SRv6 SID config:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}

		By("Verifying L3VPN routes received from TOR")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				out, err := exec.Exec("vtysh", "-c", "show bgp ipv4 vpn json")
				if err != nil {
					return fmt.Errorf("failed to query bgp ipv4 vpn on %s: %w", pod.Name, err)
				}
				if !strings.Contains(out, "192.168.20.0/24") {
					return fmt.Errorf("L3VPN route 192.168.20.0/24 not found on %s, output: %s", pod.Name, out)
				}
				return nil
			}, 3*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should create L2VNI with L3VPN routing domain", func() {
		l2vni := v1alpha1.L2VNI{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "red110",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.L2VNISpec{
				VNI:           110,
				RoutingDomain: l3vpnRoutingDomain("red"),
				HostMaster: &v1alpha1.HostMaster{
					Type:        v1alpha1.LinuxBridge,
					LinuxBridge: &v1alpha1.LinuxBridgeConfig{AutoCreate: new(true)},
				},
			},
		}
		Expect(Updater.Update(config.Resources{
			L2VNIs: []v1alpha1.L2VNI{l2vni},
		})).To(Succeed())

		By("Verifying L2VNI configuration in FRR running config")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return err
				}
				if !strings.Contains(cfg, "vni 110") {
					return fmt.Errorf("FRR config on %s does not contain vni 110:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})

// --- RawFRRConfig scenario ---

var _ = Describe("QEMU RawFRRConfig", Ordered, QEMUSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty())

		By("Creating basic underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuEVPNUnderlay()},
		})).To(Succeed())
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

	It("should inject raw config into FRR", func() {
		rawConfig := v1alpha1.RawFRRConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "qemu-test",
				Namespace: openperouter.Namespace,
			},
			Spec: v1alpha1.RawFRRConfigSpec{
				RawConfig: "ip prefix-list QEMU-TEST seq 10 permit 10.99.0.0/16",
			},
		}
		Expect(Updater.Update(config.Resources{
			RawFRRConfigs: []v1alpha1.RawFRRConfig{rawConfig},
		})).To(Succeed())

		By("Verifying raw config appears in FRR running config")
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return err
				}
				if !strings.Contains(cfg, "QEMU-TEST") {
					return fmt.Errorf("FRR config on %s does not contain QEMU-TEST prefix-list:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}

		By("Deleting RawFRRConfig and verifying removal")
		cli := Updater.Client()
		Expect(cli.Delete(context.Background(), &rawConfig)).To(Succeed())

		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return err
				}
				if strings.Contains(cfg, "QEMU-TEST") {
					return fmt.Errorf("FRR config on %s still contains QEMU-TEST after deletion:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})
