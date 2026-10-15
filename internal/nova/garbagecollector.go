// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// Default TTLs, used when the corresponding field on GarbageCollector is zero.
const (
	// DefaultOrphanTTL is how long a VirtualMachine carrying no Nova status may
	// exist (measured from creation) before it is pruned as an orphan.
	DefaultOrphanTTL = time.Hour
	// DefaultDeletionTTL is the grace window between marking a VM for deletion
	// (Nova reported DELETED) and actually deleting it.
	DefaultDeletionTTL = time.Hour
)

// Prune outcome label values recorded on prune_total.
const (
	pruneOutcomeMarked       = "marked"        // VM scheduled for deferred deletion
	pruneOutcomeUnmarked     = "unmarked"      // deferred deletion cancelled (server live again)
	pruneOutcomePruned       = "pruned"        // VM deleted after its deletion window elapsed
	pruneOutcomePrunedOrphan = "pruned_orphan" // VM deleted after the orphan TTL elapsed
	pruneOutcomeError        = "error"         // a prune operation returned an error
)

// GarbageCollector watches VirtualMachine resources and prunes them under two
// conditions, each gated behind an explicit TTL:
//
//   - Nova reports the server as DELETED. Rather than deleting immediately, the
//     VM is marked for deletion DeletionTTL into the future (Status.DeletionTime)
//     and only removed once that window elapses. If Nova reports the server live
//     again before then, the mark is cleared and the VM survives.
//   - The VM carries no Nova status at all and is older than OrphanTTL. Such a
//     resource is either a stale orphan or a slow/stalled sync; the age gate
//     distinguishes the two — a freshly created resource (empty Nova status
//     because it has not synced yet) is granted the full TTL before it becomes
//     eligible, so pruning never races the sync controller.
//
// It is intentionally separate from Controller: the sync controller only
// records Nova's view, while deletion — an irreversible, outward-facing action —
// is concentrated here behind explicit, time-gated conditions.
type GarbageCollector struct {
	client.Client
	Scheme *runtime.Scheme

	// OrphanTTL is how long a VirtualMachine carrying no Nova status may exist
	// (measured from its creation timestamp) before it is pruned. Zero selects
	// DefaultOrphanTTL.
	OrphanTTL time.Duration

	// DeletionTTL is the grace window between marking a VM for deletion (Nova
	// reported DELETED) and actually deleting it. Zero selects
	// DefaultDeletionTTL.
	DeletionTTL time.Duration

	// Metrics records prune instrumentation. May be nil (e.g. in tests), in
	// which case recording is a no-op.
	Metrics *Metrics
}

// Reconcile prunes a VirtualMachine under one of two time-gated conditions: a
// Nova-reported DELETED server (after a deferred DeletionTTL grace window) or a
// resource carrying no Nova status that has outlived OrphanTTL. Every other
// state — including a freshly created or not-yet-synced resource within its
// grace period — is left untouched and requeued for re-evaluation when it next
// becomes eligible.
func (c *GarbageCollector) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	orphanTTL := c.OrphanTTL
	if orphanTTL == 0 {
		orphanTTL = DefaultOrphanTTL
	}
	deletionTTL := c.DeletionTTL
	if deletionTTL == 0 {
		deletionTTL = DefaultDeletionTTL
	}

	var vm v1alpha1.VirtualMachine
	if err := c.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Already being deleted: nothing to do.
	if !vm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	now := metav1.Now()

	// Nova has observed the server as DELETED: schedule a deferred deletion
	// rather than removing the resource immediately, so a transient or false
	// DELETED (e.g. a server that gets re-adopted) can be recovered within the
	// grace window.
	if vm.Status.Nova != nil && vm.Status.Nova.Status == "DELETED" {
		switch {
		case vm.Status.DeletionTime == nil:
			// First observation: mark for deletion DeletionTTL from now.
			deleteAt := metav1.NewTime(now.Add(deletionTTL))
			vm.Status.DeletionTime = &deleteAt
			if err := c.Status().Update(ctx, &vm); err != nil {
				c.Metrics.recordPrune(pruneOutcomeError)
				return ctrl.Result{}, err
			}
			log.Info("marking VirtualMachine for deletion: nova reports server deleted",
				"name", vm.Name, "deletionTime", deleteAt.Time)
			c.Metrics.recordPrune(pruneOutcomeMarked)
			return ctrl.Result{RequeueAfter: deletionTTL}, nil
		case now.After(vm.Status.DeletionTime.Time):
			// Grace window elapsed: prune. Deletion is irreversible, so log at
			// Info to leave a visible audit trail.
			log.Info("pruning VirtualMachine: deletion window elapsed", "name", vm.Name)
			if err := client.IgnoreNotFound(c.Delete(ctx, &vm)); err != nil {
				c.Metrics.recordPrune(pruneOutcomeError)
				return ctrl.Result{}, err
			}
			c.Metrics.recordPrune(pruneOutcomePruned)
			return ctrl.Result{}, nil
		default:
			// Window still open: revisit exactly when it elapses.
			return ctrl.Result{RequeueAfter: vm.Status.DeletionTime.Sub(now.Time)}, nil
		}
	}

	// Nova no longer reports the server as DELETED but a deletion was scheduled:
	// the server came back (re-adopted or resurrected). Cancel the deletion.
	if vm.Status.DeletionTime != nil {
		vm.Status.DeletionTime = nil
		if err := c.Status().Update(ctx, &vm); err != nil {
			c.Metrics.recordPrune(pruneOutcomeError)
			return ctrl.Result{}, err
		}
		log.Info("cancelling scheduled deletion: nova reports server live again", "name", vm.Name)
		c.Metrics.recordPrune(pruneOutcomeUnmarked)
		return ctrl.Result{}, nil
	}

	// No Nova status at all: either a stale orphan or a slow/stalled sync. Prune
	// once the resource has outlived OrphanTTL (measured from creation), which
	// grants a freshly created resource the full window to sync before it
	// becomes eligible — so this never races the sync controller.
	if vm.Status.Nova == nil || vm.Status.Nova.Status == "" {
		age := now.Sub(vm.CreationTimestamp.Time)
		if age >= orphanTTL {
			log.Info("pruning VirtualMachine: no nova status past orphan TTL", "name", vm.Name, "age", age)
			if err := client.IgnoreNotFound(c.Delete(ctx, &vm)); err != nil {
				c.Metrics.recordPrune(pruneOutcomeError)
				return ctrl.Result{}, err
			}
			c.Metrics.recordPrune(pruneOutcomePrunedOrphan)
			return ctrl.Result{}, nil
		}
		// Not yet eligible: revisit when the orphan TTL would elapse.
		return ctrl.Result{RequeueAfter: orphanTTL - age}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the garbage collector as a watch-driven reconciler
// on VirtualMachine objects.
func (c *GarbageCollector) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("nil manager")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachine{}).
		Named("nova-cleanup").
		Complete(c)
}
