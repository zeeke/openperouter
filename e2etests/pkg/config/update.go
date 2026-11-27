// SPDX-License-Identifier:Apache-2.0

package config

import (
	"context"
	"fmt"
	"time"

	frrk8sv1beta1 "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/frr"
	"github.com/openperouter/openperouter/e2etests/pkg/ipfamily"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Resources struct {
	Underlays         []v1alpha1.Underlay      `json:"underlays"`
	L3VNIs            []v1alpha1.L3VNI         `json:"l3vnis"`
	L2VNIs            []v1alpha1.L2VNI         `json:"l2vnis"`
	L2Trunks          []v1alpha1.L2Trunk       `json:"l2trunks"`
	L3VPNs            []v1alpha1.L3VPN         `json:"l3vpns"`
	L3Passthrough     []v1alpha1.L3Passthrough `json:"l3passthrough"`
	RawFRRConfigs     []v1alpha1.RawFRRConfig  `json:"rawfrrconfigs"`
	FRRConfigurations []frrk8sv1beta1.FRRConfiguration
}

const (
	vpnDrainTimeout      = 2 * time.Minute
	vpnDrainPollInterval = 2 * time.Second
)

type Updater struct {
	cli             client.Client
	openpeNamespace string
	frrk8sNamespace string
	clientset       clientset.Interface
	hostMode        bool
}

func UpdaterForCRs(r *rest.Config, openpeNs, frrk8sNs string, hostMode bool) (*Updater, error) {
	myScheme := runtime.NewScheme()

	if err := v1alpha1.AddToScheme(myScheme); err != nil {
		return nil, err
	}

	if err := corev1.AddToScheme(myScheme); err != nil {
		return nil, err
	}

	if err := frrk8sv1beta1.AddToScheme(myScheme); err != nil {
		return nil, err
	}

	cl, err := client.New(r, client.Options{
		Scheme: myScheme,
	})

	if err != nil {
		return nil, err
	}

	cs, err := clientset.NewForConfig(r)
	if err != nil {
		return nil, err
	}

	return &Updater{
		cli:             cl,
		openpeNamespace: openpeNs,
		frrk8sNamespace: frrk8sNs,
		clientset:       cs,
		hostMode:        hostMode,
	}, nil
}

func (o Updater) Update(r Resources) error {
	// we fill a map of objects to keep the order we add the resources random, as
	// it would happen by throwing a set of manifests against a cluster, hoping to
	// find corner cases that we would not find by adding them always in the same
	// order.
	objects := map[int]client.Object{}
	oldValues := map[int]client.Object{}
	key := 0
	for _, underlay := range r.Underlays {
		objects[key] = underlay.DeepCopy()
		oldValues[key] = underlay.DeepCopy()
		key++
	}
	for _, vni := range r.L3VNIs {
		objects[key] = vni.DeepCopy()
		oldValues[key] = vni.DeepCopy()
		key++
	}
	for _, vni := range r.L2VNIs {
		objects[key] = vni.DeepCopy()
		oldValues[key] = vni.DeepCopy()
		key++
	}
	for _, trunk := range r.L2Trunks {
		objects[key] = trunk.DeepCopy()
		oldValues[key] = trunk.DeepCopy()
		key++
	}
	for _, vpn := range r.L3VPNs {
		objects[key] = vpn.DeepCopy()
		oldValues[key] = vpn.DeepCopy()
		key++
	}
	for _, l3Passthrough := range r.L3Passthrough {
		objects[key] = l3Passthrough.DeepCopy()
		oldValues[key] = l3Passthrough.DeepCopy()
		key++
	}
	for _, rawFRRConfig := range r.RawFRRConfigs {
		objects[key] = rawFRRConfig.DeepCopy()
		oldValues[key] = rawFRRConfig.DeepCopy()
		key++
	}
	for _, frrConfig := range r.FRRConfigurations {
		objects[key] = frrConfig.DeepCopy()
		oldValues[key] = frrConfig.DeepCopy()
		key++
	}

	// Iterating over the map will return the items in a random order.
	for i, obj := range objects {
		switch obj.(type) {
		case *frrk8sv1beta1.FRRConfiguration:
			obj.SetNamespace(o.frrk8sNamespace)
		default:
			obj.SetNamespace(o.openpeNamespace)
		}

		_, err := controllerutil.CreateOrUpdate(context.Background(), o.cli, obj, func() error {
			// the mutate function is expected to change the object when updating.
			// we always override with the old version, and we change only the spec part.
			switch toChange := obj.(type) {
			case *v1alpha1.Underlay:
				old := oldValues[i].(*v1alpha1.Underlay)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.L3VNI:
				old := oldValues[i].(*v1alpha1.L3VNI)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.L2VNI:
				old := oldValues[i].(*v1alpha1.L2VNI)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.L2Trunk:
				old := oldValues[i].(*v1alpha1.L2Trunk)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.L3VPN:
				old := oldValues[i].(*v1alpha1.L3VPN)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.L3Passthrough:
				old := oldValues[i].(*v1alpha1.L3Passthrough)
				toChange.Spec = *old.Spec.DeepCopy()
			case *v1alpha1.RawFRRConfig:
				old := oldValues[i].(*v1alpha1.RawFRRConfig)
				toChange.Spec = *old.Spec.DeepCopy()
			case *frrk8sv1beta1.FRRConfiguration:
				old := oldValues[i].(*frrk8sv1beta1.FRRConfiguration)
				toChange.Spec = *old.Spec.DeepCopy()
			default:
				return fmt.Errorf("unsupported object, namespace: %s, name: %s, type: %T",
					obj.GetNamespace(), obj.GetName(), obj)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// CleanAll deletes all relevant resources in the namespace.
// The underlay is deleted last, and only once the routers withdrew their VPN
// routes: removing it tears down the default BGP instance, and FRR leaves the
// instance in a half-deleted hidden state when VPN routes are still in the RIB,
// crashing bgpd as soon as the instance is recreated (FRRouting/frr#23835).
func (o Updater) CleanAll() error {
	if err := o.CleanButUnderlay(); err != nil {
		return err
	}
	underlayList := v1alpha1.UnderlayList{}
	if err := o.cli.List(context.Background(), &underlayList,
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if len(underlayList.Items) == 0 {
		return nil
	}
	if err := o.waitForVPNRoutesWithdrawal(); err != nil {
		return fmt.Errorf("waiting for VPN routes to be withdrawn before removing the underlay: %w", err)
	}
	return o.cli.DeleteAllOf(context.Background(), &v1alpha1.Underlay{},
		client.InNamespace(o.openpeNamespace))
}

// waitForVPNRoutesWithdrawal polls every router until both VPN tables are
// empty. CleanAll is an operation invoked from setup paths, not an assertion,
// so it owns its own bounded polling instead of leaving it to gomega.
func (o Updater) waitForVPNRoutesWithdrawal() error {
	routers, err := openperouter.Get(o.clientset, o.hostMode)
	if err != nil {
		return fmt.Errorf("getting routers: %w", err)
	}

	deadline := time.Now().Add(vpnDrainTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = nil
		withdrawn := true
		for exec := range routers.GetExecutors() {
			if err := vpnTablesEmpty(exec); err != nil {
				withdrawn = false
				lastErr = err
				break
			}
		}
		if withdrawn {
			return nil
		}
		time.Sleep(vpnDrainPollInterval)
	}
	return fmt.Errorf("timed out, %w", lastErr)
}

func vpnTablesEmpty(exec openperouter.RouterExecutor) error {
	for _, family := range []ipfamily.Family{ipfamily.IPv4, ipfamily.IPv6} {
		data, err := frr.L3VPNInfo(exec, family)
		if err != nil {
			return fmt.Errorf("querying %s vpn table on router %s: %w", family, exec.Name(), err)
		}
		routes := 0
		for _, prefixes := range data.Routes.RouteDistinguishers {
			routes += len(prefixes)
		}
		if routes > 0 {
			return fmt.Errorf("router %s still holds %d %s vpn routes", exec.Name(), routes, family)
		}
	}
	return nil
}

// CleanButUnderlay deletes all resources but the underlays.
// This is needed as deleting underlays is a time consuming operation that
// will cause the router pods to be recreated.
func (o Updater) CleanButUnderlay() error {
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.L3VNI{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.L2VNI{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.L2Trunk{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.L3VPN{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.L3Passthrough{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &v1alpha1.RawFRRConfig{},
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if err := o.cli.DeleteAllOf(context.Background(), &frrk8sv1beta1.FRRConfiguration{},
		client.InNamespace(o.frrk8sNamespace)); err != nil {
		return err
	}
	return nil
}

// IsUnderlayClean returns an error if listing of underlays fails or if any underlay CR exists.
func (o Updater) IsUnderlayClean() error {
	underlayList := v1alpha1.UnderlayList{}
	if err := o.cli.List(context.Background(), &underlayList,
		client.InNamespace(o.openpeNamespace)); err != nil {
		return err
	}
	if len(underlayList.Items) > 0 {
		return fmt.Errorf("underlay list is not empty, existing: %+v", underlayList.Items)
	}
	return nil
}

func (o Updater) Client() client.Client {
	return o.cli
}

func (o Updater) Namespace() string {
	return o.openpeNamespace
}
