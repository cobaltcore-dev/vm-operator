// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// DefaultOrphanTTL is how long a resource whose Nova fields have been cleared
// (its server is gone) is retained before the cleanup controller prunes it.
const DefaultOrphanTTL = 30 * time.Minute

// CleanupController watches VirtualMachine resources and prunes those the Nova
// sync controller has marked as no longer backed by a live server:
//
//   - the Nova server was observed as DELETED: pruned immediately, and
//   - the Nova fields were cleared (server vanished) and stayed empty for
//     longer than OrphanTTL: pruned once the TTL elapses.
//
// It is intentionally separate from Controller: the sync controller only
// records Nova's view, while deletion — an irreversible, outward-facing action —
// is concentrated here behind an explicit TTL, so a transient Nova blip cannot
// cascade into mass deletions.
type CleanupController struct {
	client.Client
	Scheme *runtime.Scheme

	// OrphanTTL overrides DefaultOrphanTTL when set.
	OrphanTTL time.Duration
}

// Reconcile prunes a VirtualMachine if Nova considers its server gone. A
// DELETED server is pruned at once; a resource whose Nova fields were cleared is
// pruned once it has been orphaned for OrphanTTL, otherwise the request is
// requeued for the remaining time.
func (c *CleanupController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var vm v1alpha1.VirtualMachine
	if err := c.Get(ctx, req.NamespacedName, &vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Already being deleted: nothing to do.
	if !vm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Nova observed the server as DELETED: prune immediately.
	novaReportsDeleted := vm.Status.Nova != nil && vm.Status.Nova.Status == "DELETED"
	if novaReportsDeleted {
		log.V(1).Info("pruning VirtualMachine: nova reports server deleted", "name", vm.Name)
		return ctrl.Result{}, client.IgnoreNotFound(c.Delete(ctx, &vm))
	}

	// Orphan: the sync controller cleared the Nova fields because the server
	// vanished. Prune once it has been orphaned longer than the TTL, else
	// requeue for the remaining time.
	isOrphan := !novaFieldsSet(&vm)
	if isOrphan {
		// Measure the orphan age from the last successful sync (when the fields
		// were cleared), falling back to the creation time.
		orphanedSince := vm.CreationTimestamp
		if vm.Status.Nova != nil && vm.Status.Nova.LastSyncTime != nil {
			orphanedSince = *vm.Status.Nova.LastSyncTime
		}
		orphanTTL := DefaultOrphanTTL
		if c.OrphanTTL > 0 {
			orphanTTL = c.OrphanTTL
		}
		if age := time.Since(orphanedSince.Time); age < orphanTTL {
			return ctrl.Result{RequeueAfter: orphanTTL - age}, nil
		}
		log.V(1).Info("pruning VirtualMachine: orphaned past ttl", "name", vm.Name, "ttl", orphanTTL)
		return ctrl.Result{}, client.IgnoreNotFound(c.Delete(ctx, &vm))
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the cleanup controller as a watch-driven
// reconciler on VirtualMachine objects.
func (c *CleanupController) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("nil manager")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachine{}).
		Named("nova-cleanup").
		Complete(c)
}
