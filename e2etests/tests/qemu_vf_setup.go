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

	It("should have an igb NIC bound to vfio-pci", func() {
		pod := controllerPods[0]
		exec := executor.ForPod(pod.Namespace, pod.Name, "controller")

		out, err := exec.Exec("sh", "-c",
			`for pci in /sys/bus/pci/devices/0000:*; do
				driver=$(basename "$(readlink "$pci/driver" 2>/dev/null)" 2>/dev/null)
				vendor=$(cat "$pci/vendor" 2>/dev/null)
				# 8086:10c9 is Intel 82576 (igb)
				device=$(cat "$pci/device" 2>/dev/null)
				if [ "$driver" = "vfio-pci" ] && [ "$vendor" = "0x8086" ]; then
					echo "$(basename $pci)"
					exit 0
				fi
			done
			echo "NONE"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).NotTo(Equal("NONE"), "expected one igb NIC bound to vfio-pci")
	})

	It("should have a kernel-bound igb NIC with a network interface", func() {
		pod := controllerPods[0]
		exec := executor.ForPod(pod.Namespace, pod.Name, "controller")

		out, err := exec.Exec("sh", "-c",
			`count=0
			for pci in /sys/bus/pci/drivers/igb/0000:*; do
				if ls "$pci/net/" >/dev/null 2>&1; then
					count=$((count + 1))
				fi
			done
			echo "$count"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).NotTo(Equal("0"), "expected at least one kernel-bound igb NIC with a net interface")
	})
})
