// SPDX-License-Identifier:Apache-2.0

package conversion

import (
	"fmt"
	"syscall"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/grout"
)

func ValidateGroutL3Passthrough(l3Passthrough v1alpha1.L3Passthrough) error {
	return nil
}

func ValidateGroutL3VNI(l3VNI v1alpha1.L3VNI) error {
	return nil
}

func ValidateGroutL2VNI(l2VNI v1alpha1.L2VNI) error {
	return fmt.Errorf("L2VNI resources are not supported when grout datapath is enabled")
}

func ValidateGroutUnderlay(underlay v1alpha1.Underlay) error {
	for _, iface := range underlay.Spec.Interfaces {
		if iface.Type == v1alpha1.UnderlayInterfaceTypeCNIDevice {
			return fmt.Errorf("CNI dev underlays are not supported with the grout datapath")
		}
	}

	underlayInterfaces, err := underlayInterfacesToHost(underlay.Spec.Interfaces)
	if err != nil {
		return err
	}
	for _, iface := range underlayInterfaces {
		// An explicit portName is bounded by the CRD, so only the default
		// "u_<nic>" name can overflow.
		if portName := grout.PortName(iface); len(portName) >= syscall.IFNAMSIZ {
			return fmt.Errorf("nic name %s can't be longer than %d characters, as its grout port name %s "+
				"adds the %q prefix: set acceleratedConfig.portName to use a longer nic",
				iface.InterfaceName, syscall.IFNAMSIZ-1-len(grout.UnderlayPortNamePrefix), portName,
				grout.UnderlayPortNamePrefix)
		}
	}
	return nil
}
