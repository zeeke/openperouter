// SPDX-License-Identifier:Apache-2.0
package tests

// import (
// 	"time"

// 	. "github.com/onsi/ginkgo/v2"
// 	. "github.com/onsi/gomega"
// 	"github.com/openperouter/openperouter/api/v1alpha1"
// 	"github.com/openperouter/openperouter/e2etests/pkg/config"
// 	"github.com/openperouter/openperouter/e2etests/pkg/frrk8s"
// 	"github.com/openperouter/openperouter/e2etests/pkg/infra"
// 	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
// 	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
// 	corev1 "k8s.io/api/core/v1"
// 	clientset "k8s.io/client-go/kubernetes"
// )

// var (
// 	// NOTE: we can't advertise any ip via EVPN from the leaves, they
// 	// must be reacheable otherwise FRR will skip them.
// 	leafADefaultPrefixes = []string{"192.170.20.0/24"}
// 	leafBDefaultPrefixes = []string{"192.170.21.0/24"}
// )

// var _ = Describe("Routes between bgp and the fabric", Label("underlay"), func() {
// 	var cs clientset.Interface
// 	var routers openperouter.Routers

// 	BeforeAll(func() {
// 		err := Updater.CleanAll()
// 		Expect(err).NotTo(HaveOccurred())

// 		cs = k8sclient.New()
// 		routers, err = openperouter.Get(cs, HostMode)
// 		Expect(err).NotTo(HaveOccurred())

// 		routers.Dump(GinkgoWriter)

// 		if SkipUnderlayPassthrough {
// 			return
// 		}
// 		err = Updater.Update(config.Resources{
// 			Underlays: []v1alpha1.Underlay{
// 				infra.Underlay,
// 			},
// 		})
// 		Expect(err).NotTo(HaveOccurred())
// 	})

// 	AfterAll(func() {
// 		if SkipUnderlayPassthrough {
// 			err := Updater.CleanButUnderlay()
// 			Expect(err).NotTo(HaveOccurred())
// 			return
// 		}

// 		err := Updater.CleanAll()
// 		Expect(err).NotTo(HaveOccurred())
// 		By("waiting for the router pod to rollout after removing the underlay")
// 		Eventually(func() error {
// 			newRouters, err := openperouter.Get(cs, HostMode)
// 			if err != nil {
// 				return err
// 			}
// 			return openperouter.DaemonsetRolled(routers, newRouters)
// 		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
// 	})

// 	It("are correctly exchanged", func() {
// 		err := Updater.CleanAll()
// 		Expect(err).NotTo(HaveOccurred())

// 		cs = k8sclient.New()
// 		routers, err = openperouter.Get(cs, HostMode)
// 		Expect(err).NotTo(HaveOccurred())

// 		err = Updater.Update(config.Resources{
// 			Underlays: []v1alpha1.Underlay{
// 				infra.Underlay,
// 			},
// 		})
// 		Expect(err).NotTo(HaveOccurred())

// 		ShouldExist := true
// 		frrk8sPods := []*corev1.Pod{}
// 		frrK8sConfigRed, err := frrk8s.ConfigFromHostSession(passthrough.Spec.HostSession, passthrough.Name)
// 		if err != nil {
// 			panic(err)
// 		}
// 		frrK8sConfigBlue, err := frrk8s.ConfigFromHostSession(passthrough.Spec.HostSession, passthrough.Name)
// 		if err != nil {
// 			panic(err)
// 		}

// 		BeforeEach(func() {
// 			frrk8sPods, err = frrk8s.Pods(cs)
// 			Expect(err).NotTo(HaveOccurred())

// 			DumpPods("FRRK8s pods", frrk8sPods)

// 			err = Updater.Update(config.Resources{
// 				L3Passthrough: []v1alpha1.L3Passthrough{
// 					passthrough,
// 				},
// 				FRRConfigurations: append(frrK8sConfigRed, frrK8sConfigBlue...),
// 			})
// 			Expect(err).NotTo(HaveOccurred())

// 			validateFRRK8sSessionForHostSession(passthrough.Name, passthrough.Spec.HostSession, Established, frrk8sPods...)
// 		})

// 		AfterEach(func() {
// 			dumpIfFails(cs)
// 			err := Updater.CleanButUnderlay()
// 			Expect(err).NotTo(HaveOccurred())
// 			Expect(infra.LeafAConfig.RemovePrefixes()).To(Succeed())
// 			Expect(infra.LeafBConfig.RemovePrefixes()).To(Succeed())
// 		})

// 		It("translates BGP incoming routes as BGP routes", func() {

// 			By("advertising routes from both leaves")
// 			Expect(infra.LeafAConfig.ChangePrefixes(leafADefaultPrefixes, emptyPrefixes, emptyPrefixes)).To(Succeed())
// 			Expect(infra.LeafBConfig.ChangePrefixes(leafBDefaultPrefixes, emptyPrefixes, emptyPrefixes)).To(Succeed())

// 			By("checking routes are propagated via BGP")

// 			for _, frrk8s := range frrk8sPods {
// 				checkBGPPrefixesForHostSession(frrk8s, passthrough.Spec.HostSession, leafADefaultPrefixes, ShouldExist)
// 				checkBGPPrefixesForHostSession(frrk8s, passthrough.Spec.HostSession, leafBDefaultPrefixes, ShouldExist)
// 			}

// 			By("removing routes from the leaf B")
// 			Expect(infra.LeafAConfig.ChangePrefixes(leafADefaultPrefixes, emptyPrefixes, emptyPrefixes)).To(Succeed())
// 			Expect(infra.LeafBConfig.ChangePrefixes(emptyPrefixes, emptyPrefixes, emptyPrefixes)).To(Succeed())

// 			By("checking routes are propagated via BGP")

// 			for _, frrk8s := range frrk8sPods {
// 				checkBGPPrefixesForHostSession(frrk8s, passthrough.Spec.HostSession, leafADefaultPrefixes, ShouldExist)
// 				checkBGPPrefixesForHostSession(frrk8s, passthrough.Spec.HostSession, leafBDefaultPrefixes, !ShouldExist)
// 			}
// 		})
// 	})

// })
