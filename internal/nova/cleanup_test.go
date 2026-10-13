// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

func TestCleanupController_SetupWithManager_NilManager(t *testing.T) {
	if err := (&CleanupController{}).SetupWithManager(nil); err == nil {
		t.Fatal("expected SetupWithManager(nil) to return an error, got nil")
	}
}

func TestCleanupController_prunesDeletedServer(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "DELETED"}
	c := &CleanupController{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected immediate prune (no requeue), got %v", res.RequeueAfter)
	}
	assertDeleted(t, c.Client, name)
}

// A resource whose Nova fields were never populated (a slow or stalled sync, or
// a freshly created resource) must survive: an empty resource is NOT the same
// as a deleted server. This is the race the controller must not lose.
func TestCleanupController_keepsUnsyncedVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	// No Nova fields at all, created long ago — the former orphan-TTL path
	// would have pruned this; it must now be left alone.
	vm.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	c := &CleanupController{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("unsynced VM should not be requeued, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("unsynced VM should survive, got err=%v", err)
	}
}

// A resource whose Nova fields were cleared (server vanished but Nova never
// reported DELETED) must also survive, for the same reason.
func TestCleanupController_keepsClearedVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	vm.Status.Nova = &v1alpha1.NovaStatus{
		LastSyncTime: new(metav1.NewTime(time.Now().Add(-24 * time.Hour))),
	}
	c := &CleanupController{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("cleared VM should not be requeued, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("cleared VM should survive, got err=%v", err)
	}
}

func TestCleanupController_keepsHealthyVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1"})
	c := &CleanupController{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("healthy VM should not be requeued, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("healthy VM should survive, got err=%v", err)
	}
}

// assertDeleted fails the test unless the named VM is gone.
func assertDeleted(t *testing.T, c client.Reader, name string) {
	t.Helper()
	var vm v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &vm); !apierrors.IsNotFound(err) {
		t.Fatalf("expected %s to be deleted, got err=%v", name, err)
	}
}
