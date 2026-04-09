// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"net"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

func setupTAPInHostNamespace(ctx context.Context, names hostnetwork.VethNames) error {
	// TODO: implement TAP interface setup for grout
	return nil
}

// assignIPsToGroutPort assigns IPv4 and IPv6 addresses to a grout port by name.
func assignIPsToGroutPort(portName string, ipv4, ipv6 string) error {
	_ = portName
	_ = net.IP{} // will be used for IP parsing
	// TODO: implement IP assignment to grout ports via grcli
	return nil
}
