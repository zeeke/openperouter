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

var _ = Describe("QEMU L3Passthrough with Underlay", QEMUSupport, Ordered, func() {
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
					Type:          "NetworkDevice",
					NetworkDevice: &v1alpha1.NetworkDevice{InterfaceName: "enp1s0"},
				},
			},
			Neighbors: []v1alpha1.Neighbor{
				{
					ASN:     new(int64(65000)),
					Address: new("192.168.100.1"),
				},
			},
		},
	}

	passthrough := v1alpha1.L3Passthrough{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "passthrough",
			Namespace: openperouter.Namespace,
		},
		Spec: v1alpha1.L3PassthroughSpec{
			HostSession: v1alpha1.HostSession{
				ASN:     64514,
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
		cs = k8sclient.New()

		var err error
		routerPods, err = openperouter.RouterPods(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty(), "no router pods found")
		DumpPods("Router pods", routerPods)
	})

	AfterAll(func() {
		if !QEMUMode {
			return
		}
		cli := Updater.Client()
		p := passthrough.DeepCopy()
		p.Namespace = openperouter.Namespace
		_ = cli.Delete(context.Background(), p)
		u := qemuUnderlay.DeepCopy()
		u.Namespace = openperouter.Namespace
		_ = cli.Delete(context.Background(), u)
	})

	It("should create the underlay with QEMU network parameters", func() {
		err := Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{qemuUnderlay},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should configure FRR with the TOR neighbor", func() {
		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return fmt.Errorf("failed to get FRR running config from %s: %w", pod.Name, err)
				}
				if !strings.Contains(cfg, "neighbor 192.168.100.1") {
					return fmt.Errorf("FRR config on %s does not contain TOR neighbor 192.168.100.1:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})

	It("should establish BGP session with the TOR", func() {
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

	It("should create L3Passthrough and configure host session in FRR", func() {
		err := Updater.Update(config.Resources{
			L3Passthrough: []v1alpha1.L3Passthrough{passthrough},
		})
		Expect(err).NotTo(HaveOccurred())

		for _, pod := range routerPods {
			exec := openperouter.ExecutorForPod(pod)
			Eventually(func() error {
				cfg, err := frr.RunningConfig(exec)
				if err != nil {
					return fmt.Errorf("failed to get FRR running config from %s: %w", pod.Name, err)
				}
				if !strings.Contains(cfg, "192.169.10.") {
					return fmt.Errorf("FRR config on %s does not contain host session CIDR:\n%s", pod.Name, cfg)
				}
				return nil
			}, 2*time.Minute, 5*time.Second).ShouldNot(HaveOccurred())
		}
	})
})
