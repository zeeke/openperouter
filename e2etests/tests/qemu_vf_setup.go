// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var _ = Describe("QEMU VF Setup Verification", QEMUSupport, Ordered, func() {
	var cs clientset.Interface
	var controllerPods []*corev1.Pod

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		cs = k8sclient.New()

		var err error
		controllerPods, err = openperouter.ControllerPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(controllerPods).NotTo(BeEmpty(), "no controller pods found")
	})

	It("should have a VF bound to vfio-pci", func() {
		pod := controllerPods[0]
		exec := executor.ForPod(pod.Namespace, pod.Name, "controller")

		out, err := exec.Exec("sh", "-c",
			`PF=$(ls -d /sys/bus/pci/drivers/igb/0000:* 2>/dev/null | head -1)
			VF=$(readlink -f "$PF/virtfn0")
			basename $(readlink "$VF/driver")`)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(Equal("vfio-pci"), "VF0 should be bound to vfio-pci")
	})

	It("should have a VF network interface available", func() {
		pod := controllerPods[0]
		exec := executor.ForPod(pod.Namespace, pod.Name, "controller")

		out, err := exec.Exec("sh", "-c",
			`PF=$(ls -d /sys/bus/pci/drivers/igb/0000:* 2>/dev/null | head -1)
			VF=$(readlink -f "$PF/virtfn1")
			ls "$VF/net/" 2>/dev/null | head -1`)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).NotTo(BeEmpty(), "VF1 should have a network interface")
	})
})
