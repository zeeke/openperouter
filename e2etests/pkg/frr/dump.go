// SPDX-License-Identifier:Apache-2.0

package frr

import (
	"errors"
	"fmt"

	"github.com/openperouter/openperouter/e2etests/pkg/executor"
)

func RawDump(exec executor.Executor) (string, error) {
	allerrs := errors.New("")
	res := "####### Show version\n"
	out, err := exec.Exec("vtysh", "-c", "show version")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show version: %v", err))
	}
	res += out

	res += "####### Show running config\n"
	out, err = exec.Exec("vtysh", "-c", "show running-config")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show bgp neighbor: %v", err))
	}
	res += out

	res += "####### BGP Neighbors\n"
	out, err = exec.Exec("vtysh", "-c", "show bgp vrf all neighbor")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show bgp neighbor: %v", err))
	}
	res += out

	res += "####### Routes\n"
	out, err = exec.Exec("vtysh", "-c", "show bgp vrf all ipv4")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show bgp ipv4: %v", err))
	}
	res += out

	res += "####### Routes\n"
	out, err = exec.Exec("vtysh", "-c", "show bgp vrf all ipv6")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show bgp ipv6: %v", err))
	}
	res += out

	res += "####### EVPN Routes\n"
	out, err = exec.Exec("vtysh", "-c", "show bgp l2vpn evpn")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec show bgp ipv6: %v", err))
	}
	res += out

	res += "####### Network setup for host\n"
	out, err = exec.Exec("bash", "-c", "ip -6 route; ip -4 route")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec to print network setup: %v", err))
	}
	res += out
	out, err = exec.Exec("bash", "-c", "ip l")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec to print network setup: %v", err))
	}
	res += out
	out, err = exec.Exec("bash", "-c", "ip address")
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec to print network setup: %v", err))
	}
	res += out

	out, err = dumpGroutInfo(exec)
	if err != nil {
		allerrs = errors.Join(allerrs, fmt.Errorf("\nFailed exec to print grout setup: %v", err))
	}
	res += out

	if allerrs.Error() == "" {
		allerrs = nil
	}

	return res, allerrs
}

func dumpGroutInfo(exec executor.Executor) (string, error) {
	out, err := exec.Exec("grcli", "--version")
	if err != nil {
		return "", nil
	}

	ret := "\n\nGrout info:\n"

	ret = "Grout interface:\n"
	out, err = exec.Exec("grcli", "interface", "show")
	if err != nil {
		return "", nil
	}
	ret += out + "\n"

	ret += "Grout address:\n"
	out, err = exec.Exec("grcli", "address", "show")
	if err != nil {
		return "", nil
	}
	ret += out + "\n"

	ret += "Grout route:\n"
	out, err = exec.Exec("grcli", "route", "show")
	if err != nil {
		return "", nil
	}
	ret += out + "\n"

	ret += "Grout nexthop:\n"
	out, err = exec.Exec("grcli", "nexthop", "show")
	if err != nil {
		return "", nil
	}
	ret += out + "\n"

	ret += "Grout stats:\n"
	out, err = exec.Exec("grcli", "stats", "show")
	if err != nil {
		return "", nil
	}
	ret += out + "\n"

	return ret, nil
}
