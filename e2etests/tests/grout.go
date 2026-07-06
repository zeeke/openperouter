// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"encoding/json"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

type groutInterface struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

var _ = Describe("Grout datapath", GroutSupport, Ordered, func() {
	var cs clientset.Interface
	var routers openperouter.Routers

	vni := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "red",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3VNISpec{
			VRF: "red",
			HostSession: &v1alpha1.HostSession{
				ASN:     64514,
				HostASN: new(int64(64515)),
				LocalCIDR: v1alpha1.LocalCIDRConfig{
					IPv4: new("192.169.10.0/24"),
				},
			},
			VNI: 100,
		},
	}

	BeforeAll(func() {
		err := Updater.CleanAll()
		Expect(err).NotTo(HaveOccurred())

		cs = k8sclient.New()
		Eventually(func() error {
			routers, err = openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())

		err = Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{
				infra.Underlay,
			},
			L3VNIs: []v1alpha1.L3VNI{vni},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() error {
			routers, err = openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
	})

	AfterAll(func() {
		err := Updater.CleanAll()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		dumpIfFails(cs)
	})

	It("should have grout interfaces configured", func() {
		for router := range routers.GetExecutors() {
			By("checking grout interfaces on " + router.Name())
			out, err := router.Exec("grcli", "--err-exit", "--json", "interface", "show")
			Expect(err).NotTo(HaveOccurred(), "grcli interface show failed on %s: %s", router.Name(), out)

			var ifaces []groutInterface
			Expect(json.Unmarshal([]byte(out), &ifaces)).To(Succeed(), "failed to parse grcli output on %s", router.Name())

			types := map[string]bool{}
			for _, iface := range ifaces {
				types[iface.Type] = true
			}

			Expect(types).To(HaveKey("vrf"), "no grout VRF found on %s", router.Name())
			Expect(types).To(HaveKey("vxlan"), "no grout VXLAN found on %s", router.Name())
			Expect(types).To(HaveKey("port"), "no grout port found on %s", router.Name())
		}
	})

	It("should not have kernel datapath objects in the perouter netns", func() {
		for router := range routers.GetExecutors() {
			By("checking kernel interfaces are absent on " + router.Name())
			for _, linkType := range []string{"vrf", "bridge", "vxlan"} {
				out, err := router.Exec("ip", "link", "show", "type", linkType)
				if err != nil && strings.Contains(out, "does not exist") {
					continue
				}
				Expect(err).NotTo(HaveOccurred(), "ip link show type %s failed on %s", linkType, router.Name())
				Expect(strings.TrimSpace(out)).To(BeEmpty(),
					"kernel %s interface found on %s, expected grout datapath only", linkType, router.Name())
			}
		}
	})
})
