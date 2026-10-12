// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// Controller reconciles VirtualMachine objects.
//
// It is currently a no-op skeleton: it observes the VM CRD and returns without
// taking any action. Nova synchronization logic will be added here.
//
// Client is typically a cortex multicluster client, so reads and writes may be
// served by the home cluster or a remote cluster routed by availability zone
// (see VirtualMachineRouter).
//
// The manager needs the following RBAC permissions for this controller; they
// are templated by hand in dist/templates/rbac.yaml rather than generated:
//   - cobaltcore.cloud/virtualmachines: get;list;watch;create;update;patch;delete
//   - cobaltcore.cloud/virtualmachines/status: get;update;patch
//   - cobaltcore.cloud/virtualmachines/finalizers: update
type Controller struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is the core reconciliation loop for a single VirtualMachine.
//
// It does nothing yet beyond fetching the object and tolerating deletions.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var vm v1alpha1.VirtualMachine
	if err := c.Get(ctx, req.NamespacedName, &vm); err != nil {
		// The VM was deleted after the reconcile request was enqueued; nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.V(1).Info("reconciling VirtualMachine", "name", vm.Name)

	// TODO: implement Nova synchronization.
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager so it reconciles
// on VirtualMachine objects.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.VirtualMachine{}).
		Named("nova").
		Complete(c)
}
