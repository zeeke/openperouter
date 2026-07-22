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

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

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
				if [ "$driver" = "vfio-pci" ] && [ "$vendor" = "0x8086" ]; then
					echo "RESULT:$(basename $pci)"
					exit 0
				fi
			done
			echo "RESULT:NONE"`)
		Expect(err).NotTo(HaveOccurred())
		result := lastLine(out)
		Expect(result).To(HavePrefix("RESULT:"))
		Expect(strings.TrimPrefix(result, "RESULT:")).NotTo(Equal("NONE"),
			"expected one igb NIC bound to vfio-pci")
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
			echo "RESULT:$count"`)
		Expect(err).NotTo(HaveOccurred())
		result := lastLine(out)
		Expect(result).To(HavePrefix("RESULT:"))
		Expect(strings.TrimPrefix(result, "RESULT:")).NotTo(Equal("0"),
			"expected at least one kernel-bound igb NIC with a net interface")
	})
})
