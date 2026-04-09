// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/vishvananda/netlink"
)

func SetupPassthrough(ctx context.Context, params hostnetwork.PassthroughParams) error {
	// TODO: implement grout-based passthrough setup

	return nil
}

func RemovePassthrough(targetNS string) error {
	// TODO: implement grout-based passthrough removal
	return nil
}

func removeLinkByName(name string) error {
	link, err := netlink.LinkByName(name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove link by name: failed to get link %s: %w", name, err)
	}
	return netlink.LinkDel(link)
}
