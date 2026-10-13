// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// CleanupController watches VirtualMachine resources and prunes those whose Nova
// server the sync controller observed as DELETED.
//
// It is intentionally separate from Controller: the sync controller only
// records Nova's view, while deletion — an irreversible, outward-facing action —
// is concentrated here behind a single, explicit condition.
//
// Deletion is gated solely on Nova reporting the server as DELETED. In
// particular, a resource whose Nova fields are empty is NOT pruned: those
// fields are empty both when a server has vanished and when the sync controller
// simply has not populated them yet (a freshly created resource, or a slow or
// stalled sync). The two cases are indistinguishable from the resource alone,
// so treating an empty resource as deletable would race the sync controller and
// discard VirtualMachines whose servers are still live in Nova.
type CleanupController struct {
	client.Client
	Scheme *runtime.Scheme

	// Metrics records prune instrumentation. May be nil (e.g. in tests), in
	// which case recording is a no-op.
	Metrics *Metrics
}

// Reconcile prunes a VirtualMachine only once Nova has observed its server in
// the DELETED state. Any other state — including a resource whose Nova fields
// have not yet been synced — is left untouched.
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

	// Only prune when Nova has explicitly observed the server as DELETED. An
	// empty Nova status means "not yet synced", not "server gone", so it is
	// deliberately left alone.
	if vm.Status.Nova != nil && vm.Status.Nova.Status == "DELETED" {
		// Pruning deletes the resource — an irreversible action — so log it at
		// Info (not V(1)) to leave a visible audit trail in normal operation. It
		// is low-frequency, so this does not add steady-state noise.
		log.Info("pruning VirtualMachine: nova reports server deleted", "name", vm.Name)
		if err := client.IgnoreNotFound(c.Delete(ctx, &vm)); err != nil {
			c.Metrics.recordPrune("error")
			return ctrl.Result{}, err
		}
		c.Metrics.recordPrune("pruned")
		return ctrl.Result{}, nil
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
