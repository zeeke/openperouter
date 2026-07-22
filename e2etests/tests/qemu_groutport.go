// SPDX-License-Identifier:Apache-2.0

package tests

import (
	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("GroutPort DPDK underlay", QEMUSupport, Ordered, func() {
	BeforeAll(func() {
		if !QEMUMode {
			Skip("QEMU mode not enabled")
		}
	})

	// These tests require the GroutPort API types to be implemented.
	// They exercise the SR-IOV VF passthrough flow end-to-end on the
	// QEMU VM with emulated igb hardware.

	It("should create an underlay with GroutPort (PCI address)", func() {
		Skip("GroutPort API not yet implemented")
	})

	It("should establish BGP session with TOR via DPDK port", func() {
		Skip("GroutPort API not yet implemented")
	})

	It("should assign inline IPAM addresses to grout port", func() {
		Skip("GroutPort API not yet implemented")
	})

	It("should tear down GroutPort on underlay deletion", func() {
		Skip("GroutPort API not yet implemented")
	})
})
