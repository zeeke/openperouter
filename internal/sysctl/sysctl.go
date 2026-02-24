// SPDX-License-Identifier:Apache-2.0

package sysctl

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netns"
)

// Sysctl represents a sysctl setting to be applied.
type Sysctl struct {
	Path        string // The sysctl path under /proc/sys/
	Value       string // The desired value ("0" or "1")
	Description string // Human-readable description for logging
}

// Ensure applies the given sysctls in the target namespace.
// Each sysctl is checked and only written if not already at the desired value.
func Ensure(namespace string, sysctls ...Sysctl) error {
	ns, err := netns.GetFromPath(namespace)
	if err != nil {
		return fmt.Errorf("failed to get network namespace %s: %w", namespace, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("Ensure: failed to close namespace", "error", err, "namespace", namespace)
		}
	}()

	err = netnamespace.In(ns, func() error {
		for _, s := range sysctls {
			desiredValue := s.Value
			if desiredValue == "" {
				desiredValue = "1" // default to enable for backward compatibility
			}
			path := "/proc/sys/" + s.Path
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("failed to read %s: %w", path, err)
			}
			currentValue := strings.TrimSpace(string(data))

			if currentValue != desiredValue {
				if err := os.WriteFile(path, []byte(desiredValue), 0644); err != nil {
					return fmt.Errorf("failed to write to %s: %w", path, err)
				}
				slog.Info("sysctl set", "path", s.Path, "value", desiredValue, "description", s.Description, "namespace", namespace)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to ensure sysctls: %w", err)
	}
	return nil
}

// IPv4Forwarding returns the sysctl definition for enabling IPv4 forwarding.
func IPv4Forwarding() Sysctl {
	return Sysctl{
		Path:        "net/ipv4/conf/all/forwarding",
		Value:       "1",
		Description: "IPv4 forwarding",
	}
}

// IPv6Forwarding returns the sysctl definition for enabling IPv6 forwarding.
func IPv6Forwarding() Sysctl {
	return Sysctl{
		Path:        "net/ipv6/conf/all/forwarding",
		Value:       "1",
		Description: "IPv6 forwarding",
	}
}

// DisableIPv4Forwarding returns the sysctl definition for disabling IPv4 forwarding.
// Used when grout handles forwarding in the DPDK dataplane.
func DisableIPv4Forwarding() Sysctl {
	return Sysctl{
		Path:        "net/ipv4/conf/all/forwarding",
		Value:       "0",
		Description: "IPv4 forwarding disabled (grout dataplane)",
	}
}

// DisableIPv6Forwarding returns the sysctl definition for disabling IPv6 forwarding.
// Used when grout handles forwarding in the DPDK dataplane.
func DisableIPv6Forwarding() Sysctl {
	return Sysctl{
		Path:        "net/ipv6/conf/all/forwarding",
		Value:       "0",
		Description: "IPv6 forwarding disabled (grout dataplane)",
	}
}

// ArpAcceptAll returns the sysctl definition for enabling arp_accept on all
// running interfaces. Enabling arp_accept allows the kernel to create neighbor entries
// from received Gratuitous ARP packets, which is critical for fast EVPN MAC/IP route
// advertisement during VM migrations.
func ArpAcceptAll() Sysctl {
	return Sysctl{
		Path:        "net/ipv4/conf/all/arp_accept",
		Value:       "1",
		Description: "arp_accept on all interfaces",
	}
}

// ArpAcceptDefault returns the sysctl definition for enabling arp_accept on
// newly created interfaces. This ensures that any new interface will inherit the
// arp_accept setting.
func ArpAcceptDefault() Sysctl {
	return Sysctl{
		Path:        "net/ipv4/conf/default/arp_accept",
		Value:       "1",
		Description: "arp_accept on new interfaces",
	}
}

// AcceptUntrackedNAAll returns the sysctl definition for enabling accept_untracked_na.
// This is the IPv6 equivalent of arp_accept - it allows the kernel to create neighbor entries
// from received unsolicited Neighbor Advertisement packets, which is critical for fast EVPN
// MAC/IP route advertisement during VM migrations with IPv6.
// Note: This sysctl is only available on kernels >= 5.18.
func AcceptUntrackedNAAll() Sysctl {
	return Sysctl{
		Path:        "net/ipv6/conf/all/accept_untracked_na",
		Value:       "1",
		Description: "accept_untracked_na on all interfaces",
	}
}

// AcceptUntrackedNADefault returns the sysctl definition for enabling accept_untracked_na on
// newly created interfaces. This ensures that any new interface will inherit the setting.
// This is the IPv6 equivalent of arp_accept - it allows the kernel to create neighbor entries
// from received unsolicited Neighbor Advertisement packets, which is critical for fast EVPN
// MAC/IP route advertisement during VM migrations with IPv6.
// Note: This sysctl is only available on kernels >= 5.18.
func AcceptUntrackedNADefault() Sysctl {
	return Sysctl{
		Path:        "net/ipv6/conf/default/accept_untracked_na",
		Value:       "1",
		Description: "accept_untracked_na on new interfaces",
	}
}
