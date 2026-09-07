// SPDX-License-Identifier:Apache-2.0

package conversion

import (
	"strings"
	"testing"

	"github.com/openperouter/openperouter/api/v1alpha1"
)

func TestKernelDatapathRejectsSRIOVVFPair(t *testing.T) {
	pci := "0000:03:02.0"
	validator := &KernelDatapathConfigValidator{}
	err := validator.Validate(APIConfigData{
		L2VNIs: []v1alpha1.L2VNI{
			{
				Spec: v1alpha1.L2VNISpec{
					VNI: 100,
					SRIOVVFPair: &v1alpha1.SRIOVVFPairConfig{
						PCIAddress: &pci,
						VLAN:       10,
					},
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for sriovVFPair on kernel datapath, got nil")
	}
	if !strings.Contains(err.Error(), "sriovVFPair requires grout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestKernelDatapathAllowsL2VNIWithoutSRIOVVFPair(t *testing.T) {
	validator := &KernelDatapathConfigValidator{}
	err := validator.Validate(APIConfigData{
		L2VNIs: []v1alpha1.L2VNI{
			{
				Spec: v1alpha1.L2VNISpec{VNI: 100},
			},
		},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}
