// SPDX-License-Identifier:Apache-2.0

package qemu_e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/ipfamily"
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/networklayerprotocol"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	"github.com/openshift-kni/k8sreporter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

var (
	Updater     *config.Updater
	HostMode    bool
	GroutMode   bool
	ReportPath  string
	k8sReporter *k8sreporter.KubernetesReporter
)

var GroutSupport = ginkgo.Label("grout-support")
var QEMUSupport = ginkgo.Label("qemu-support")

const Established = true

type validationParameters struct {
	fromName                string
	toName                  string
	neighborIP              string
	receivedAddressFamilies []networklayerprotocol.NLP
	established             bool
}

func validateSessionWithNeighbor(exec executor.Executor, parameters validationParameters) {
	Eventually(func() error {
		neigh, err := frr.NeighborInfo(parameters.neighborIP, exec)
		if err != nil {
			return err
		}
		if !parameters.established && neigh.BgpState == "Established" {
			return fmt.Errorf("neighbor from %s to %s - %s is established", parameters.fromName, parameters.toName, parameters.neighborIP)
		}
		if parameters.established && neigh.BgpState != "Established" {
			return fmt.Errorf("neighbor %s to %s - %s is not established", parameters.fromName, parameters.toName, parameters.neighborIP)
		}

		if !parameters.established {
			return nil
		}
		for _, expectedReceivedAF := range parameters.receivedAddressFamilies {
			isRxReceived := false
			for pathName, addPath := range neigh.NeighborCapabilities.AddPath {
				if strings.ToLower(pathName) == fmt.Sprintf("%s%s", expectedReceivedAF.AFI, expectedReceivedAF.SAFI) {
					isRxReceived = addPath.RxReceived
					break
				}
			}
			if isRxReceived {
				continue
			}
			return fmt.Errorf("neighbor %s to %s - %s is established but expectedReceivedAF %s not found",
				parameters.fromName, parameters.toName, parameters.neighborIP, expectedReceivedAF)
		}

		return nil
	}, 5*time.Minute, time.Second).ShouldNot(HaveOccurred())
}

func waitForType5Route(exec executor.Executor, prefix string) {
	Eventually(func() error {
		evpn, err := frr.EVPNInfo(exec)
		if err != nil {
			return err
		}
		if !evpn.ContainsType5Prefix(prefix) {
			return fmt.Errorf("Type-5 route for %s not yet present", prefix)
		}
		return nil
	}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())
}

func l3vniRoutingDomain(name string) *v1alpha1.RoutingDomain {
	return &v1alpha1.RoutingDomain{
		Type:  v1alpha1.RoutingDomainTypeL3VNI,
		L3VNI: &v1alpha1.L3VNIReference{Name: name},
	}
}

type groutInterface struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func canPingFromPod(exec executor.Executor, ip string) {
	ginkgo.GinkgoHelper()
	Eventually(func(g Gomega) {
		ginkgo.By(fmt.Sprintf("pinging %s via net1", ip))
		out, err := exec.Exec("ping", "-c", "1", "-W", "2", "-I", "net1", ip)
		g.Expect(err).ToNot(HaveOccurred(), "ping to %s failed: %s", ip, out)
	}).
		WithTimeout(40 * time.Second).
		WithPolling(time.Second).
		Should(Succeed())
}

func removeGatewayFromPod(pod *corev1.Pod) error {
	exec := executor.ForPod(pod.Namespace, pod.Name, "agnhost")

	var podIPs []string
	for _, podIP := range pod.Status.PodIPs {
		podIPs = append(podIPs, podIP.IP)
	}

	family, err := ipfamily.ForAddresses(podIPs...)
	if err != nil {
		return fmt.Errorf("failed to detect IP family for pod %s: %w", pod.Name, err)
	}

	if family == ipfamily.IPv4 || family == ipfamily.DualStack {
		output, err := exec.Exec("ip", "route", "del", "default", "dev", "eth0")
		if err != nil {
			return fmt.Errorf("failed to remove ipv4 gateway from pod %s: %s: %w", pod.Name, output, err)
		}
	}

	if family == ipfamily.IPv6 || family == ipfamily.DualStack {
		nextHopIPv6, err := findNextHopIPv6(exec, "default", "eth0")
		if err != nil {
			return fmt.Errorf("failed to find IPv6 next hop for pod %s: %w", pod.Name, err)
		}
		output, err := exec.Exec("ip", "-6", "route", "del", "default", "via", nextHopIPv6, "dev", "eth0")
		if err != nil {
			return fmt.Errorf("failed to remove ipv6 gateway from pod %s: %s: %w", pod.Name, output, err)
		}
	}

	return nil
}

func findNextHopIPv6(exec executor.Executor, destination, device string) (string, error) {
	output, err := exec.Exec("ip", "-6", "route", "show", destination)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(fmt.Sprintf(`via +([0-9a-fA-F:]+) dev %s`, device))
	match := re.FindStringSubmatch(output)
	if len(match) == 0 {
		return "", fmt.Errorf("cannot extract ipv6 default gateway for dev eth0 from output: %s", output)
	}
	return strings.TrimSpace(match[1]), nil
}

func DumpPods(name string, pods []*corev1.Pod) {
	ginkgo.GinkgoWriter.Printf("%s pods are:", name)
	for _, pod := range pods {
		ginkgo.GinkgoWriter.Printf("Pod %s/%s: %s", pod.Namespace, pod.Name, pod.Status.Phase)
		ginkgo.GinkgoWriter.Printf("  Node: %s", pod.Spec.NodeName)
		ginkgo.GinkgoWriter.Printf("  IPs: %v", pod.Status.PodIPs)
		ginkgo.GinkgoWriter.Printf("  Containers:")
		for _, c := range pod.Spec.Containers {
			ginkgo.GinkgoWriter.Printf("    - %s: %s", c.Name, c.Image)
		}
		ginkgo.GinkgoWriter.Print("\n")
	}
}

func dumpIfFails(cs clientset.Interface, additionalNamespaces ...string) {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}

	routers, err := openperouter.Get(cs, HostMode)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("dumpIfFails: failed to get routers: %v", err)
		return
	}

	testPath, err := createTestOutput(ReportPath, ginkgo.CurrentSpecReport().FullText())
	if err != nil {
		ginkgo.GinkgoWriter.Printf("dumpIfFails: failed to create test dir: %v", err)
		return
	}

	for router := range routers.GetExecutors() {
		func() {
			var dump strings.Builder
			dump.WriteString(frr.RawDump(router) + "\n\n")
			if GroutMode {
				dump.WriteString(frr.GroutDump(router) + "\n\n")
			}

			f, err := logFileFor(testPath, fmt.Sprintf("frrdump-%s", router.Name()))
			if err != nil {
				ginkgo.GinkgoWriter.Printf("dumpIfFails: failed to open file for %s: %v", router.Name(), err)
				return
			}
			defer f.Close()
			fmt.Fprintf(f, "Dumping information for %s\n", router.Name())
			fmt.Fprint(f, dump.String())
		}()
	}

	for _, namespace := range additionalNamespaces {
		dumpWorkloadInfo(testPath, cs, namespace)
	}

	k8s.DumpInfo(k8sReporter, ginkgo.CurrentSpecReport().FullText())
}

func dumpWorkloadInfo(testPath string, cs clientset.Interface, namespace string) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		ginkgo.GinkgoWriter.Printf("dumpWorkloadInfo: failed to list pods in namespace %s: %v", namespace, err)
		return
	}

	for _, pod := range pods.Items {
		if len(pod.Spec.Containers) == 0 {
			continue
		}
		container := pod.Spec.Containers[0]
		exec := executor.ForPod(pod.Namespace, pod.Name, container.Name)
		func() {
			var res strings.Builder
			commands := []struct {
				desc string
				cmd  []string
			}{
				{"ip link", []string{"bash", "-c", "ip l"}},
				{"ip address", []string{"bash", "-c", "ip address"}},
				{"ip route table all", []string{"bash", "-c", "ip route show table all"}},
			}
			for _, c := range commands {
				fmt.Fprintf(&res, "\n######## %s\n\n", c.desc)
				out, err := exec.Exec(c.cmd[0], c.cmd[1:]...)
				if err != nil {
					fmt.Fprintf(&res, "\nFailed exec %q: %v", strings.Join(c.cmd, " "), err)
				}
				res.WriteString(out)
			}

			f, err := logFileFor(testPath, fmt.Sprintf("pod-dump-%s-%s", namespace, pod.Name))
			if err != nil {
				ginkgo.GinkgoWriter.Printf("dumpWorkloadInfo: failed to open file for pod %s: %v", pod.Name, err)
				return
			}
			defer f.Close()
			fmt.Fprint(f, res.String())
		}()
	}
}

func createTestOutput(basePath, testName string) (string, error) {
	nonAlphanumeric := regexp.MustCompile(`[^a-zA-Z0-9]+`)
	sanitizedName := nonAlphanumeric.ReplaceAllString(testName, "_")
	testPath := path.Join(basePath, sanitizedName)
	err := os.Mkdir(testPath, 0755)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("failed to create test dir: %w", err)
	}
	return testPath, nil
}

func logFileFor(base string, kind string) (*os.File, error) {
	path := path.Join(base, kind) + ".log"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return f, nil
}
