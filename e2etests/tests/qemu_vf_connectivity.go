// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var _ = Describe("QEMU FakeVF Connectivity", QEMUSupport, Ordered, func() {
	var cs clientset.Interface
	var controllerPods []*corev1.Pod
	var exec executor.Executor
	var vfIfaces []string

	const (
		vfAIP = "10.100.100.1/24"
		vfBIP = "10.100.100.2/24"
	)

	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
		cs = k8sclient.New()

		var err error
		controllerPods, err = openperouter.ControllerPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(controllerPods).NotTo(BeEmpty(), "no controller pods found")

		exec = executor.ForPod(controllerPods[0].Namespace, controllerPods[0].Name, "controller")

		// Find kernel-bound igb NICs that are NOT the underlay (no IP assigned).
		out, err := exec.Exec("sh", "-c", `
for pci in /sys/bus/pci/drivers/igb/0000:*; do
	iface=$(ls "$pci/net/" 2>/dev/null | head -1)
	[ -z "$iface" ] && continue
	# skip interfaces with an IP address (the underlay NIC)
	if ip -4 addr show "$iface" 2>/dev/null | grep -q "inet "; then
		continue
	fi
	echo "$iface"
done`)
		Expect(err).NotTo(HaveOccurred())

		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			iface := strings.TrimSpace(line)
			if iface != "" {
				vfIfaces = append(vfIfaces, iface)
			}
		}
		Expect(len(vfIfaces)).To(BeNumerically(">=", 2),
			"need at least 2 kernel-bound igb interfaces (excluding underlay), found: %v", vfIfaces)

		GinkgoWriter.Printf("Using fakeVF interfaces: %s and %s\n", vfIfaces[0], vfIfaces[1])
	})

	AfterAll(func() {
		if exec == nil || len(vfIfaces) < 2 {
			return
		}
		for _, iface := range vfIfaces[:2] {
			exec.Exec("ip", "addr", "flush", "dev", iface)
			exec.Exec("ip", "link", "set", iface, "down")
		}
	})

	It("should assign addresses and bring up two fakeVF interfaces", func() {
		for _, cmd := range []struct {
			iface, ip string
		}{
			{vfIfaces[0], vfAIP},
			{vfIfaces[1], vfBIP},
		} {
			_, err := exec.Exec("ip", "link", "set", cmd.iface, "up")
			Expect(err).NotTo(HaveOccurred())
			_, err = exec.Exec("ip", "addr", "add", cmd.ip, "dev", cmd.iface)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("should allow fakeVF-to-fakeVF ping", func() {
		vfBAddr, _, _ := strings.Cut(vfBIP, "/")
		Eventually(func() error {
			out, err := exec.Exec("ping", "-c", "1", "-W", "2", "-I", vfIfaces[0], vfBAddr)
			if err != nil {
				return fmt.Errorf("ping from %s to %s failed: %s: %w", vfIfaces[0], vfBAddr, out, err)
			}
			return nil
		}, 30*time.Second, time.Second).Should(Succeed(),
			"fakeVF %s should reach fakeVF %s", vfIfaces[0], vfIfaces[1])
	})

	It("should allow fakeVF-to-fakeVF ping in reverse direction", func() {
		vfAAddr, _, _ := strings.Cut(vfAIP, "/")
		Eventually(func() error {
			out, err := exec.Exec("ping", "-c", "1", "-W", "2", "-I", vfIfaces[1], vfAAddr)
			if err != nil {
				return fmt.Errorf("ping from %s to %s failed: %s: %w", vfIfaces[1], vfAAddr, out, err)
			}
			return nil
		}, 30*time.Second, time.Second).Should(Succeed(),
			"fakeVF %s should reach fakeVF %s", vfIfaces[1], vfIfaces[0])
	})
})
