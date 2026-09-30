// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// newTestReconciler builds a Controller backed by a fake client seeded with
// the given objects.
func newTestReconciler(t *testing.T, objs ...client.Object) *Controller {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	return &Controller{Client: c, Scheme: scheme}
}

func TestController_Reconcile(t *testing.T) {
	const vmName = "63ed64e2-27a2-4b70-9b5c-833c2a285d66"

	existingVM := &v1alpha1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: vmName},
		Spec: v1alpha1.VMSpec{
			Region: "qa-de-1",
			AZ:     "qa-de-1a",
		},
	}

	tests := []struct {
		name    string
		objects []client.Object
		request ctrl.Request
	}{
		{
			name:    "existing VM reconciles without error",
			objects: []client.Object{existingVM},
			request: ctrl.Request{NamespacedName: types.NamespacedName{Name: vmName}},
		},
		{
			// The VM was deleted after the request was enqueued; the reconciler
			// must tolerate the not-found and not error.
			name:    "missing VM is tolerated",
			objects: nil,
			request: ctrl.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReconciler(t, tc.objects...)

			result, err := r.Reconcile(context.Background(), tc.request)
			if err != nil {
				t.Fatalf("Reconcile returned unexpected error: %v", err)
			}

			// The controller is a no-op skeleton: it never requeues.
			if result != (ctrl.Result{}) {
				t.Errorf("expected empty result, got %+v", result)
			}
		})
	}
}

func TestController_SetupWithManager_NilManager(t *testing.T) {
	// SetupWithManager must fail rather than panic when given no manager. This
	// guards the wiring called from cmd/main.go.
	c := &Controller{}
	if err := c.SetupWithManager(nil); err == nil {
		t.Fatal("expected SetupWithManager(nil) to return an error, got nil")
	}
}
