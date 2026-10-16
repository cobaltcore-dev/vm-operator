// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

func TestGarbageCollector_SetupWithManager_NilManager(t *testing.T) {
	if err := (&GarbageCollector{}).SetupWithManager(nil); err == nil {
		t.Fatal("expected SetupWithManager(nil) to return an error, got nil")
	}
}

// A DELETED server is not pruned immediately: the first observation schedules a
// deferred deletion DeletionTTL into the future and leaves the resource in
// place, so a transient/false DELETED can still be recovered.
func TestGarbageCollector_marksDeletedServer(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "DELETED"}
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != DefaultDeletionTTL {
		t.Errorf("expected requeue after %v, got %v", DefaultDeletionTTL, res.RequeueAfter)
	}

	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("VM should survive the mark, got err=%v", err)
	}
	if got.Status.DeletionTime == nil {
		t.Fatal("expected DeletionTime to be set, got nil")
	}
	if d := time.Until(got.Status.DeletionTime.Time); d <= 0 || d > DefaultDeletionTTL+time.Minute {
		t.Errorf("DeletionTime should be ~%v out, got %v", DefaultDeletionTTL, d)
	}
}

// Once the deletion window has elapsed, a DELETED server is pruned.
func TestGarbageCollector_prunesAfterDeletionWindow(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "DELETED"}
	vm.Status.DeletionTime = &past
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected immediate prune (no requeue), got %v", res.RequeueAfter)
	}
	assertDeleted(t, c.Client, name)
}

// While the deletion window is still open, a DELETED server is left in place and
// requeued for when the window elapses.
func TestGarbageCollector_waitsForDeletionWindow(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	future := metav1.NewTime(time.Now().Add(30 * time.Minute))
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "DELETED"}
	vm.Status.DeletionTime = &future
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected a positive requeue while window open, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("VM should survive while window open, got err=%v", err)
	}
}

// A scheduled deletion is cancelled when Nova reports the server live again
// before the window elapses.
func TestGarbageCollector_cancelsDeletionWhenServerReturns(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	future := metav1.NewTime(time.Now().Add(30 * time.Minute))
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "ACTIVE", Host: "compute-1"}
	vm.Status.DeletionTime = &future
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue after cancelling, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatalf("VM should survive, got err=%v", err)
	}
	if got.Status.DeletionTime != nil {
		t.Errorf("expected DeletionTime to be cleared, got %v", got.Status.DeletionTime)
	}
}

// A resource carrying no Nova status that has outlived the orphan TTL is pruned.
func TestGarbageCollector_prunesOldOrphan(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	// No Nova status at all, created well beyond the orphan TTL.
	vm.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected immediate prune (no requeue), got %v", res.RequeueAfter)
	}
	assertDeleted(t, c.Client, name)
}

// A freshly created resource with no Nova status must survive: the empty status
// means "not yet synced", not "orphan". It is requeued for when the orphan TTL
// would elapse. This is the race the collector must not lose.
func TestGarbageCollector_keepsFreshOrphan(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	vm.CreationTimestamp = metav1.Now()
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > DefaultOrphanTTL {
		t.Errorf("fresh orphan should requeue within the orphan TTL, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("fresh orphan should survive, got err=%v", err)
	}
}

// A server that is gone from Nova (NovaSynced=True/ServerGone) but still carries
// its last-known Nova data is pruned once past the orphan TTL — the retained data
// no longer blocks pruning; the condition drives it.
func TestGarbageCollector_prunesGoneServerPastOrphanTTL(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	// Retains last-known Nova data (Status ACTIVE), yet is marked gone.
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1"})
	vm.CreationTimestamp = metav1.NewTime(time.Now().Add(-24 * time.Hour))
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.ConditionTypeNovaSynced,
		Status: metav1.ConditionTrue,
		Reason: v1alpha1.ConditionReasonNovaSyncedGone,
	})
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected immediate prune (no requeue), got %v", res.RequeueAfter)
	}
	assertDeleted(t, c.Client, name)
}

// A server marked gone but still within the orphan TTL survives and is requeued.
func TestGarbageCollector_keepsGoneServerWithinOrphanTTL(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1"})
	vm.CreationTimestamp = metav1.Now()
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.ConditionTypeNovaSynced,
		Status: metav1.ConditionTrue,
		Reason: v1alpha1.ConditionReasonNovaSyncedGone,
	})
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > DefaultOrphanTTL {
		t.Errorf("gone server within TTL should requeue within the orphan TTL, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("gone server within TTL should survive, got err=%v", err)
	}
}

// A VM whose last sync FAILED (NovaSynced=False/SyncError) is never pruned,
// regardless of age: Nova's view is unknown, so the server may still exist. This
// is the API-outage safety property — a 401/503 must not cascade into deletion.
func TestGarbageCollector_neverPrunesWhenSyncFailing(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1"})
	// Ancient — well past any TTL — yet must survive because sync is failing.
	vm.CreationTimestamp = metav1.NewTime(time.Now().Add(-240 * time.Hour))
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.ConditionTypeNovaSynced,
		Status:  metav1.ConditionFalse,
		Reason:  v1alpha1.ConditionReasonNovaSyncedError,
		Message: "nova unreachable",
	})
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

	res, err := c.Reconcile(context.Background(), request())
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("failing-sync VM should not be requeued by the GC, got %v", res.RequeueAfter)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Errorf("failing-sync VM must survive regardless of age, got err=%v", err)
	}
}

func TestGarbageCollector_keepsHealthyVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1"})
	c := &GarbageCollector{Client: newTestClient(t, scheme, vm), Scheme: scheme}

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
