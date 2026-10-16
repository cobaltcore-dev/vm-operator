// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	// DefaultGCInterval is how often the re-enqueue loop lists every
	// VirtualMachine and requeues it, used when GarbageCollector.Interval is
	// non-positive. It is also the --nova-gc-interval flag default (see
	// cmd/main.go). It is deliberately longer than the sync poll's DefaultInterval:
	// the loop is a cheap, Nova-free backstop and prune decisions are TTL-gated at
	// hour granularity, so a tight cadence buys nothing. The interval is clamped to
	// at least MinInterval (shared with Controller).
	DefaultGCInterval = 10 * time.Minute
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

	// Interval is how often the re-enqueue loop (Start) lists every
	// VirtualMachine and requeues it, so a deferred prune survives the loss of an
	// in-memory RequeueAfter (pod restart, leader-election failover). Clamped to
	// [MinInterval, ∞); a non-positive value falls back to DefaultGCInterval.
	Interval time.Duration

	// eventCh carries the uuids the re-enqueue loop emits; created in
	// SetupWithManager, closed by Start on shutdown.
	eventCh chan event.GenericEvent

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
				"deletionTime", deleteAt.Time)
			c.Metrics.recordPrune(pruneOutcomeMarked)
			return ctrl.Result{RequeueAfter: deletionTTL}, nil
		case now.After(vm.Status.DeletionTime.Time):
			// Grace window elapsed: prune. Deletion is irreversible, so log at
			// Info to leave a visible audit trail.
			log.Info("pruning VirtualMachine: deletion window elapsed")
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
		log.Info("cancelling scheduled deletion: nova reports server live again")
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
		log.V(1).Info("skipping prune: nova sync is failing, server state unknown")
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
			log.Info("pruning VirtualMachine: not in nova past orphan TTL", "age", age, "goneFromNova", goneFromNova)
			if err := client.IgnoreNotFound(c.Delete(ctx, &vm)); err != nil {
				c.Metrics.recordPrune(pruneOutcomeError)
				return ctrl.Result{}, err
			}
			c.Metrics.recordPrune(pruneOutcomePrunedOrphan)
			return ctrl.Result{}, nil
		}
		// Not yet eligible: revisit when the orphan TTL would elapse. This
		// RequeueAfter is in-memory only; the Start re-enqueue loop is the durable
		// backstop that re-arms it if the process restarts before it fires.
		requeueAfter := orphanTTL - age
		log.V(1).Info("not in nova but within orphan TTL, requeueing", "age", age, "requeueAfter", requeueAfter)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	return ctrl.Result{}, nil
}

// Start runs the periodic re-enqueue loop until ctx is cancelled. It implements
// manager.Runnable so the manager owns its lifecycle; it is registered via
// mgr.Add in SetupWithManager.
//
// Unlike Controller's poll loop it makes no Nova calls: it lists VirtualMachines
// from k8s and enqueues every one, letting Reconcile apply the TTL gates. This is
// the durable backstop for a deferred prune whose in-memory RequeueAfter would
// otherwise be lost on a pod restart or leader-election failover — the newly
// elected leader starts with an empty workqueue, and the startup enqueue re-arms
// every pending prune.
func (c *GarbageCollector) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("nova-gc-reenqueue")

	interval := c.Interval
	if interval <= 0 {
		interval = DefaultGCInterval
	}
	// Safety: ensure interval is at least MinInterval to prevent tight loops.
	if interval < MinInterval {
		log.Info("gc interval too low, using minimum",
			"configuredInterval", c.Interval, "minInterval", MinInterval)
		interval = MinInterval
	}

	log.Info("starting nova garbage-collector re-enqueue loop", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(c.eventCh)

	// Enqueue once immediately so a restart re-arms pending prunes without waiting
	// a full interval.
	c.enqueueAll(ctx, log)
	for {
		select {
		case <-ticker.C:
			c.enqueueAll(ctx, log)
		case <-ctx.Done():
			return nil
		}
	}
}

// enqueueAll lists every VirtualMachine and enqueues it for reconciliation. It
// makes no Nova calls and enqueues unconditionally — Reconcile decides
// eligibility (a healthy or failing-sync VM is a no-op; a within-TTL one is
// requeued). It logs (rather than returns) a list error so a transient failure
// does not tear down the loop.
func (c *GarbageCollector) enqueueAll(ctx context.Context, log logr.Logger) {
	var vmList v1alpha1.VirtualMachineList
	if err := c.List(ctx, &vmList); err != nil {
		log.Error(err, "re-enqueue loop: failed to list VirtualMachines")
		return
	}
	log.V(1).Info("re-enqueueing all VirtualMachines", "count", len(vmList.Items))
	for i := range vmList.Items {
		evt := event.GenericEvent{Object: &v1alpha1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vmList.Items[i].Name,
				Namespace: vmList.Items[i].Namespace,
			},
		}}
		select {
		case c.eventCh <- evt:
		case <-ctx.Done():
			return
		}
	}
}

// SetupWithManager wires the periodic re-enqueue loop into the manager and
// registers the reconciler to consume what it (and a predicated watch) enqueue.
//
// Two event sources feed the one reconciler:
//   - the eventCh raw source fed by the re-enqueue loop (the durable backstop);
//   - a watch on VirtualMachine objects, predicated to fire only on a transition
//     that makes a VM newly prune-relevant (Nova DELETED, gone, or never synced)
//     and to ignore the collector's own DeletionTime-only status writes, so
//     mark/cancel never re-trigger the reconciler. This picks prune-eligible
//     resources up promptly instead of waiting for the next loop tick.
func (c *GarbageCollector) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("nil manager")
	}

	c.eventCh = make(chan event.GenericEvent)
	src := source.Channel(c.eventCh, &handler.EnqueueRequestForObject{})

	// Register the re-enqueue loop so the manager starts and stops it alongside the
	// controller.
	if err := mgr.Add(c); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("nova-garbagecollector").
		WatchesRawSource(src).
		Watches(&v1alpha1.VirtualMachine{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return pruneRelevant(e.Object) },
				UpdateFunc: func(e event.UpdateEvent) bool {
					// Fire only on a prune-relevant transition; ignoring the
					// DeletionTime-only writes the collector itself makes keeps its
					// mark/cancel updates from re-triggering the reconciler.
					return pruneRelevant(e.ObjectNew) && gcInputChanged(e.ObjectOld, e.ObjectNew)
				},
				DeleteFunc:  func(event.DeleteEvent) bool { return false },  // already gone
				GenericFunc: func(event.GenericEvent) bool { return false }, // handled by the raw source
			})).
		Complete(c)
}

// pruneRelevant reports whether a VirtualMachine is a candidate the collector
// might act on: Nova reports it DELETED, Nova no longer knows it
// (NovaSynced=True/ServerGone), or it has never synced (no NovaSynced condition
// and no Nova status). Everything else — a healthy server, a failing sync — is
// left alone. A non-VirtualMachine object is never relevant.
func pruneRelevant(obj client.Object) bool {
	vm, ok := obj.(*v1alpha1.VirtualMachine)
	if !ok {
		return false
	}
	if vm.Status.Nova != nil && vm.Status.Nova.Status == "DELETED" {
		return true
	}
	synced := meta.FindStatusCondition(vm.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	goneFromNova := synced != nil && synced.Status == metav1.ConditionTrue &&
		synced.Reason == v1alpha1.ConditionReasonNovaSyncedGone
	notYetSynced := synced == nil && (vm.Status.Nova == nil || vm.Status.Nova.Status == "")
	return goneFromNova || notYetSynced
}

// gcInputChanged reports whether a field the collector dispatches on changed
// between two revisions: the Nova status string or the NovaSynced condition
// (status + reason). It deliberately ignores Status.DeletionTime, which only the
// collector itself writes — so its mark/cancel updates do not satisfy this check
// and thus never re-trigger the watch (the self-trigger an unpredicated watch
// would suffer). A non-VirtualMachine object fails open (treated as changed).
func gcInputChanged(oldObj, newObj client.Object) bool {
	oldVM, ok1 := oldObj.(*v1alpha1.VirtualMachine)
	newVM, ok2 := newObj.(*v1alpha1.VirtualMachine)
	if !ok1 || !ok2 {
		return true
	}
	// novaStatus is the Nova status string of a VM, or "" when absent.
	novaStatus := func(vm *v1alpha1.VirtualMachine) string {
		if vm.Status.Nova == nil {
			return ""
		}
		return vm.Status.Nova.Status
	}
	if novaStatus(oldVM) != novaStatus(newVM) {
		return true
	}
	oldCond := meta.FindStatusCondition(oldVM.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	newCond := meta.FindStatusCondition(newVM.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	if (oldCond == nil) != (newCond == nil) {
		return true
	}
	return oldCond != nil && newCond != nil &&
		(oldCond.Status != newCond.Status || oldCond.Reason != newCond.Reason)
}
