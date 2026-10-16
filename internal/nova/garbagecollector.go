// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
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
	// DefaultOrphanTTL is how long a VirtualMachine that is not in Nova (gone, or
	// never synced) may exist (measured from creation) before it is pruned as an
	// orphan.
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
//   - The server is not in Nova (the NovaSynced condition is True with reason
//     ServerGone) or the VM has never synced at all, and it is older than
//     OrphanTTL. The age gate (measured from creation) grants a freshly created,
//     not-yet-synced resource the full TTL before it becomes eligible, so pruning
//     never races the sync controller. A VM whose last sync FAILED (NovaSynced
//     False) is never pruned: Nova's view is unknown, so the server may still
//     exist.
//
// It is intentionally separate from Controller: the sync controller only
// records Nova's view (including marking a server gone while retaining its
// last-known fields), while deletion — an irreversible, outward-facing action —
// is concentrated here behind explicit, time-gated conditions.
type GarbageCollector struct {
	client.Client
	Scheme *runtime.Scheme

	// OrphanTTL is how long a VirtualMachine that is not in Nova (gone, or never
	// synced) may exist (measured from its creation timestamp) before it is
	// pruned. Zero selects DefaultOrphanTTL.
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
// resource that is not in Nova — gone (NovaSynced True/ServerGone) or never
// synced — that has outlived OrphanTTL. Every other state — including a freshly
// created or not-yet-synced resource within its grace period, or one whose last
// sync failed (NovaSynced False, Nova's view unknown) — is left untouched and
// requeued for re-evaluation when it next becomes eligible.
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

	// Beyond this point the server is neither DELETED nor returning from a
	// scheduled deletion. Dispatch on the NovaSynced condition, which records the
	// outcome of the last sync attempt:
	//
	//   - False (SyncError): the last fetch failed (Nova unreachable, 401/503,
	//     transport error), so we do NOT know Nova's current view — the server may
	//     well still exist. Never prune; leave it for the sync controller to retry.
	//   - True / ServerGone: a sync succeeded and Nova no longer knows the server.
	//     The last-known Nova fields are retained on the resource; prune on the
	//     OrphanTTL clock (measured from creation), which also covers a server that
	//     Cortex created but Nova has not yet persisted, or one deleted straight
	//     from the Nova DB.
	//   - absent condition: fall back to the Nova status for the pre-condition
	//     "not yet synced" case — a freshly created resource with no Nova status at
	//     all. Pruned on the same OrphanTTL clock so pruning never races the sync
	//     controller. (A resource that already carries Nova status but no condition
	//     is left alone; the next sync will set the condition.)
	synced := meta.FindStatusCondition(vm.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)

	// Last sync failed: Nova's view is unknown, so never prune. The sync
	// controller will retry and flip the condition; its status write re-triggers
	// this watch-driven reconcile when the outcome changes.
	if synced != nil && synced.Status == metav1.ConditionFalse {
		log.V(1).Info("skipping prune: nova sync is failing, server state unknown", "name", vm.Name)
		return ctrl.Result{}, nil
	}

	// Known-gone, or never synced: prune once the resource has outlived OrphanTTL
	// (measured from creation), which grants a freshly created resource the full
	// window to sync before it becomes eligible.
	goneFromNova := synced != nil && synced.Status == metav1.ConditionTrue &&
		synced.Reason == v1alpha1.ConditionReasonNovaSyncedGone
	notYetSynced := synced == nil && (vm.Status.Nova == nil || vm.Status.Nova.Status == "")
	if goneFromNova || notYetSynced {
		age := now.Sub(vm.CreationTimestamp.Time)
		if age >= orphanTTL {
			log.Info("pruning VirtualMachine: not in nova past orphan TTL", "name", vm.Name, "age", age, "goneFromNova", goneFromNova)
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
