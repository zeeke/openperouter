// SPDX-License-Identifier:Apache-2.0

package config

import (
	"context"
	"errors"
	"testing"

	frrk8sv1beta1 "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/openperouter/openperouter/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testUnderlay(name string, asn int64) v1alpha1.Underlay {
	return v1alpha1.Underlay{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openperouter-system"},
		Spec:       v1alpha1.UnderlaySpec{ASN: asn},
	}
}

func newTestUpdater(t *testing.T, objects ...client.Object) Updater {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	if err := frrk8sv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return Updater{
		cli:             cli,
		openpeNamespace: "openperouter-system",
		frrk8sNamespace: "frr-k8s",
	}
}

func TestUpdateUnderlayResetHook(t *testing.T) {
	hookErr := errors.New("hook failed")
	tests := []struct {
		name      string
		resources Resources
		objects   []client.Object
		hookErr   error
		wantHook  bool
		wantErr   error
	}{
		{
			name:      "no underlays in the resources",
			resources: Resources{},
			wantHook:  false,
		},
		{
			name: "clean cluster",
			resources: Resources{
				Underlays: []v1alpha1.Underlay{testUnderlay("underlay", 64514)},
			},
			wantHook: true,
		},
		{
			name: "same ASN already running",
			resources: Resources{
				Underlays: []v1alpha1.Underlay{testUnderlay("underlay", 64514)},
			},
			objects: []client.Object{&v1alpha1.Underlay{
				ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "openperouter-system"},
				Spec:       v1alpha1.UnderlaySpec{ASN: 64514},
			}},
			wantHook: false,
		},
		{
			name: "different ASN already running",
			resources: Resources{
				Underlays: []v1alpha1.Underlay{testUnderlay("underlay", 64514)},
			},
			objects: []client.Object{&v1alpha1.Underlay{
				ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "openperouter-system"},
				Spec:       v1alpha1.UnderlaySpec{ASN: 64520},
			}},
			wantHook: true,
		},
		{
			name: "underlay being deleted",
			resources: Resources{
				Underlays: []v1alpha1.Underlay{testUnderlay("underlay", 64514)},
			},
			objects:  []client.Object{deletingUnderlay(t, 64514)},
			wantHook: true,
		},
		{
			name: "hook failure is returned",
			resources: Resources{
				Underlays: []v1alpha1.Underlay{testUnderlay("underlay", 64514)},
			},
			hookErr:  hookErr,
			wantHook: true,
			wantErr:  hookErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := newTestUpdater(t, tt.objects...)
			hookCalled := false
			updater.OnUnderlayReset = func() error {
				hookCalled = true
				return tt.hookErr
			}

			err := updater.Update(tt.resources)

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Update() error = %v, want %v", err, tt.wantErr)
			}
			if hookCalled != tt.wantHook {
				t.Errorf("hook called = %t, want %t", hookCalled, tt.wantHook)
			}
		})
	}
}

// deletingUnderlay returns an underlay that has been deleted but is held by a
// finalizer, so its DeletionTimestamp is set.
func deletingUnderlay(t *testing.T, asn int64) *v1alpha1.Underlay {
	t.Helper()
	underlay := &v1alpha1.Underlay{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "existing",
			Namespace:  "openperouter-system",
			Finalizers: []string{"test/openperouter"},
		},
		Spec: v1alpha1.UnderlaySpec{ASN: asn},
	}
	updater := newTestUpdater(t, underlay)
	if err := updater.cli.Delete(context.Background(), underlay); err != nil {
		t.Fatalf("failed to delete the underlay: %v", err)
	}
	deleted := &v1alpha1.Underlay{ObjectMeta: metav1.ObjectMeta{
		Name: "existing", Namespace: "openperouter-system"}}
	if err := updater.cli.Get(context.Background(), client.ObjectKeyFromObject(deleted), deleted); err != nil {
		t.Fatalf("failed to get the deleting underlay: %v", err)
	}
	if deleted.DeletionTimestamp == nil {
		t.Fatal("the underlay should have a deletion timestamp")
	}
	return deleted
}

func TestCleanAllUnderlayResetHook(t *testing.T) {
	tests := []struct {
		name    string
		hookSet bool
		hookErr error
	}{
		{name: "hook not set"},
		{
			name:    "hook runs on teardown",
			hookSet: true,
		},
		{
			name:    "hook failure is returned",
			hookSet: true,
			hookErr: errors.New("hook failed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := newTestUpdater(t)
			hookCalled := false
			if tt.hookSet {
				updater.OnUnderlayReset = func() error {
					hookCalled = true
					return tt.hookErr
				}
			}

			err := updater.CleanAll()

			if !errors.Is(err, tt.hookErr) {
				t.Errorf("CleanAll() error = %v, want %v", err, tt.hookErr)
			}
			if hookCalled != tt.hookSet {
				t.Errorf("hook called = %t, want %t", hookCalled, tt.hookSet)
			}
		})
	}
}
