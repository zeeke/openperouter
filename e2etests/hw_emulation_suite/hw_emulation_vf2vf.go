// SPDX-License-Identifier:Apache-2.0

package qemu_e2e

import (
	"encoding/json"
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
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// Emulated VFs in the QEMU VM, see clab/qemu/vm/create-vfs.sh. The trunk VF
// is bound by grout, the workload VFs are access ports of the same emulated
// embedded switch on VLAN 33 or 44.
const (
	trunkVFNetlinkName = "toleafkind1v1"
	vlan33VF1          = "toleafkind1v2"
	vlan33VF2          = "toleafkind1v3"
	vlan44VF1          = "toleafkind1v4"
	vlan44VF2          = "toleafkind1v5"
	vf2vfTestNamespace = "test-vf2vf"
)

var _ = Describe("HWEmulation VF-to-VF", Ordered, QEMUSupport, GroutSupport, func() {
	var cs clientset.Interface
	var routerPods []*corev1.Pod
	var nodes []corev1.Node

	qemuUnderlay := AcceleratedUnderlay

	l3vniRed := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VNISpec{
			VRF: "red",
			VNI: 100,
			HostSession: &v1alpha1.HostSession{
				ASN:        64514,
				HostASN:    new(int64(64515)),
				LocalCIDRs: []string{"192.169.10.0/24"},
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
				TrunkVFName: trunkVFNetlinkName,
				VLAN:        33,
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
				TrunkVFName: trunkVFNetlinkName,
				VLAN:        44,
			},
		},
	}

	BeforeAll(func() {
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty(), "no router pods found")
		DumpPods("Router pods", routerPods)

		nodes, err = k8s.GetNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		By("Creating accelerated underlay")
		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuUnderlay},
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
		Expect(k8s.DeleteNamespace(cs, vf2vfTestNamespace)).To(Succeed())
		Expect(Updater.CleanAll()).To(Succeed())
		Eventually(func() error {
			routers, err := openperouter.Get(cs, false)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs, vf2vfTestNamespace)
	})

	It("should reconcile two L2VNIs sharing a trunk VF", func() {
		By("Creating the first L2VNI with sriovVFPair")
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{l3vniRed},
			L2VNIs: []v1alpha1.L2VNI{l2vniA},
		})).To(Succeed())
		openperouter.AssertNodesStatusReady(Updater.Client())
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func(g Gomega) {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				g.Expect(err).To(Succeed(), "grcli interface show failed on %s: %s", pod.Name, out)
				var ifaces []groutInterface
				g.Expect(json.Unmarshal([]byte(out), &ifaces)).To(Succeed())
				g.Expect(ifaces).To(ContainElement(And(
					HaveField("Type", "port"),
					HaveField("Name", MatchRegexp(`^t_[0-9a-z]{6}$`)),
					HaveField("Flags", ContainElement("up")),
				)))
			}, 2*time.Minute, time.Second).Should(Succeed())
		}

		By("Creating the second L2VNI on the same trunk VF")
		Expect(Updater.Update(config.Resources{L2VNIs: []v1alpha1.L2VNI{l2vniB}})).To(Succeed())
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func(g Gomega) {
				out, err := exec.Exec("grcli", "--err-exit", "--json", "interface", "show")
				g.Expect(err).To(Succeed(), "grcli interface show failed on %s: %s", pod.Name, out)
				g.Expect(out).To(MatchRegexp(`t_[0-9a-z]{6}\.33`))
				g.Expect(out).To(MatchRegexp(`t_[0-9a-z]{6}\.44`))
			}, 2*time.Minute, time.Second).Should(Succeed())
		}
		Consistently(func(g Gomega) {
			for _, node := range nodes {
				status, err := openperouter.GetNodeStatus(Updater.Client(), node.Name)
				g.Expect(err).To(Succeed())
				g.Expect(status.Status.FailedResources).To(BeEmpty(), "node %s has failed resources", node.Name)
			}
		}, 10*time.Second, time.Second).Should(Succeed())
		openperouter.AssertNodesStatusReady(Updater.Client())
	})

	It("should not have host-side bridges (no br-hs-* for VF-pair L2VNIs)", func() {
		for _, node := range nodes {
			exec := executor.ForNode(node.Name)
			out, err := exec.Exec("ip", "link", "show", "type", "bridge")
			Expect(err).NotTo(HaveOccurred(), "ip link show failed on %s: %s", node.Name, out)
			Expect(out).NotTo(ContainSubstring("br-hs-110"),
				"host bridge br-hs-110 should not exist with VF-pair mode on %s", node.Name)
			Expect(out).NotTo(ContainSubstring("br-hs-120"),
				"host bridge br-hs-120 should not exist with VF-pair mode on %s", node.Name)
		}
	})

	It("should have VXLAN interfaces for both VNIs in grout", func() {
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
					if iface.Type != "vxlan" {
						continue
					}
					if iface.Name == "vni110" {
						hasVxlan110 = true
					}
					if iface.Name == "vni120" {
						hasVxlan120 = true
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

	It("should have EVPN VNIs provisioned in VRF red", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			for _, vni := range []int{110, 120} {
				Eventually(func() error {
					info, err := frr.EVPNVNIStatus(exec, vni)
					if err != nil {
						return fmt.Errorf(
							"failed to get EVPN VNI %d status on %s: %w",
							vni, pod.Name, err,
						)
					}
					if info == nil {
						return fmt.Errorf("EVPN VNI %d not provisioned on %s", vni, pod.Name)
					}
					if info.TenantVrf != "red" {
						return fmt.Errorf(
							"EVPN VNI %d on %s is in VRF %q, expected %q",
							vni, pod.Name, info.TenantVrf, "red",
						)
					}
					return nil
				}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
			}
		}
	})

	It("should provide connectivity between pods attached to the same L2VNI", func() {
		// Each pod gets its own VF, so that same-VNI traffic crosses the
		// emulated embedded switch instead of staying inside the node.
		pods := []vfPod{
			{name: "tenant-a-1", vni: l2vniA, ip: "10.110.0.10/24", vf: vlan33VF1},
			{name: "tenant-a-2", vni: l2vniA, ip: "10.110.0.11/24", vf: vlan33VF2},
			{name: "tenant-b-1", vni: l2vniB, ip: "10.120.0.10/24", vf: vlan44VF1},
			{name: "tenant-b-2", vni: l2vniB, ip: "10.120.0.11/24", vf: vlan44VF2},
		}

		nodeName := routerPods[0].Spec.NodeName
		nodeExec := executor.ForNode(nodeName)
		for _, p := range pods {
			out, err := nodeExec.Exec("ip", "link", "set", p.vf, "up")
			Expect(err).To(Succeed(), "failed to set %s up on %s: %s", p.vf, nodeName, out)
		}

		_, err := k8s.CreateNamespace(cs, vf2vfTestNamespace)
		Expect(err).To(Succeed())

		executors := map[string]executor.Executor{}
		for _, p := range pods {
			By(fmt.Sprintf("creating pod %s in VNI %d", p.name, p.vni.Spec.VNI))
			n, err := k8s.CreateMacvlanNad(p.name, vf2vfTestNamespace, p.vf, p.vni.Spec.GatewayIPs)
			Expect(err).To(Succeed())
			pod, err := k8s.CreateAgnhostPod(cs, p.name, vf2vfTestNamespace,
				k8s.WithNad(n.Name, vf2vfTestNamespace, []string{p.ip}),
				k8s.OnNode(nodeName),
			)
			Expect(err).To(Succeed())
			executors[p.name] = executor.ForPod(pod.Namespace, pod.Name, "agnhost")
		}

		for _, from := range pods {
			By(fmt.Sprintf("pinging the gateway of VNI %d from %s", from.vni.Spec.VNI, from.name))
			canPingFromPod(executors[from.name], addressOnly(from.vni.Spec.GatewayIPs[0]))

			for _, to := range pods {
				if to.name == from.name || to.vni.Name != from.vni.Name {
					continue
				}
				By(fmt.Sprintf("pinging %s from %s", to.name, from.name))
				canPingFromPod(executors[from.name], addressOnly(to.ip))
			}
		}
	})
})

type vfPod struct {
	name string
	vni  v1alpha1.L2VNI
	ip   string
	vf   string
}

func addressOnly(cidr string) string {
	return strings.Split(cidr, "/")[0]
}
