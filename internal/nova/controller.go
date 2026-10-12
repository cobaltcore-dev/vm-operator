// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// MinInterval is the lowest polling interval the controller will honour. It
// guards against a misconfigured (zero or tiny) interval turning the periodic
// fetch into a tight loop that hammers Nova.
const MinInterval = time.Second

// DefaultInterval is used when the controller is configured with a
// non-positive Interval.
const DefaultInterval = 30 * time.Second

// Controller periodically reconciles VirtualMachine objects against Nova.
//
// Unlike a watch-driven controller, it is not triggered by changes to the VM
// CRD. Instead it runs a periodic fetch task (see Start) that asks Nova which
// servers changed, translates those server uuids into VirtualMachine reconcile
// requests, and enqueues them through eventCh. The channel is wired into the
// controller as a raw source in SetupWithManager.
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

	// OpenStack is used by the periodic fetch task to discover which Nova
	// servers changed since the last poll.
	OpenStack OpenStackClient

	// Interval is how often the periodic fetch task polls Nova. Values below
	// MinInterval are clamped; a non-positive value falls back to
	// DefaultInterval.
	Interval time.Duration

	// eventCh carries the VirtualMachine objects produced by the periodic
	// fetch task into the controller's reconcile queue. It is created in
	// SetupWithManager and closed by Start when the manager shuts down.
	eventCh chan event.GenericEvent
}

// Reconcile is the core reconciliation loop for a single VirtualMachine.
//
// It does nothing yet beyond fetching the object and tolerating deletions.
// Future work will react to the Nova-observed state here, e.g. deleting
// orphaned VM resources whose Nova server no longer exists.
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

// Start runs the periodic fetch task until ctx is cancelled.
//
// On each tick it asks Nova which servers changed and enqueues a reconcile
// request for the matching VirtualMachine (the VM CRD is named after the Nova
// server uuid). It implements manager.Runnable so the manager owns its
// lifecycle; it is registered via mgr.Add in SetupWithManager.
func (c *Controller) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("nova-fetch")

	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Safety: ensure interval is at least MinInterval to prevent tight loops.
	if interval < MinInterval {
		log.Info("fetch interval too low, using minimum",
			"configuredInterval", c.Interval, "minInterval", MinInterval)
		interval = MinInterval
	}

	log.Info("starting nova fetch task", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(c.eventCh)

	// Poll once immediately so we don't idle for a full interval on startup.
	c.poll(ctx, log)
	for {
		select {
		case <-ticker.C:
			c.poll(ctx, log)
		case <-ctx.Done():
			return nil
		}
	}
}

// poll fetches the changed Nova servers and enqueues a reconcile request for
// each. Errors are logged rather than returned so a transient Nova failure
// does not tear down the fetch task.
func (c *Controller) poll(ctx context.Context, log logr.Logger) {
	changed, err := c.OpenStack.FetchChangedServers()
	if err != nil {
		log.Error(err, "failed to fetch changed servers from nova")
		return
	}

	log.V(1).Info("fetched changed servers", "count", len(changed))
	for _, uuid := range changed {
		// The VM CRD is named after the Nova server uuid, so a GenericEvent
		// carrying a VirtualMachine with that name is translated by
		// EnqueueRequestForObject into the right reconcile request.
		evt := event.GenericEvent{
			Object: &v1alpha1.VirtualMachine{
				ObjectMeta: metav1.ObjectMeta{Name: uuid},
			},
		}
		select {
		case c.eventCh <- evt:
		case <-ctx.Done():
			return
		}
	}
}

// SetupWithManager wires the periodic fetch task into the manager and
// registers the reconciler to consume the events it produces.
//
// There is no For(&VirtualMachine{}) watch: reconciliation is driven solely by
// the eventCh raw source, fed by the Start fetch loop.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("nil manager")
	}

	c.eventCh = make(chan event.GenericEvent)
	src := source.Channel(c.eventCh, &handler.EnqueueRequestForObject{})

	// Register the periodic fetch task so the manager starts and stops it
	// alongside the controller.
	if err := mgr.Add(c); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("nova").
		WatchesRawSource(src).
		Complete(c)
}
