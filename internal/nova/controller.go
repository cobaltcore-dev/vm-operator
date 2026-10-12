// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

// Controller keeps VirtualMachine resources in sync with the Nova servers they
// mirror.
//
// It is split along the standard controller-runtime seam: a periodic poll loop
// (see Start) decides *which* resources are out of sync and enqueues their
// uuids, and Reconcile does the per-resource heavy lifting — it fetches that one
// server's authoritative state from Nova and writes it onto the resource. The
// poll loop deliberately performs no per-server Nova calls; it uses a single
// bulk listing to avoid enqueuing resources that are already in sync.
//
// Reconcile never deletes resources. Pruning is the cleanup controller's job
// (see CleanupController): Reconcile only records Nova's view, including marking
// a resource as gone when Nova no longer knows the server.
//
// Client is typically a cortex multicluster client, so reads and writes may be
// served by the home cluster or a remote cluster routed by availability zone
// (see VirtualMachineRouter).
type Controller struct {
	client.Client
	Scheme *runtime.Scheme

	// OpenStack is the Nova API the poll loop and Reconcile talk to.
	OpenStack OpenStackClient

	// Interval is how often the poll loop lists Nova. Values below MinInterval
	// are clamped; a non-positive value falls back to DefaultInterval.
	Interval time.Duration

	// eventCh carries the uuids the poll loop decides to reconcile. It is
	// created in SetupWithManager and closed by Start when the manager shuts
	// down. The channel only decides *which* resources get reconciled; the data
	// fetching happens in Reconcile.
	eventCh chan event.GenericEvent
}

// Reconcile brings a single VirtualMachine in line with Nova's current view of
// the server with the same uuid. It fetches that one server's authoritative
// state and then:
//
//   - server found, resource found: sync the Nova-derived fields onto the
//     resource (this includes a DELETED server, whose status is recorded as-is).
//   - server found, resource missing: create the resource with the fields Nova
//     provides.
//   - server missing, resource found: clear the Nova-derived fields (do NOT
//     delete — the cleanup controller prunes).
//   - server missing, resource missing: nothing to do.
//
// On success it records a NovaSynced=True condition; on a Nova fetch error it
// records NovaSynced=False and returns the error so the request is retried.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	server, found, err := c.OpenStack.FetchServer(ctx, req.Name)
	if err != nil {
		// Record the failure on the resource if it exists, then retry.
		c.markSyncFailed(ctx, req.NamespacedName, err)
		return ctrl.Result{}, fmt.Errorf("fetching nova server %s: %w", req.Name, err)
	}

	var vm v1alpha1.VirtualMachine
	getErr := c.Get(ctx, req.NamespacedName, &vm)
	switch {
	case getErr == nil:
		// Resource exists: sync (or clear) and patch.
		return ctrl.Result{}, c.syncExisting(ctx, log, &vm, server, found)
	case apierrors.IsNotFound(getErr):
		if !found {
			// Neither side has it; nothing to do.
			return ctrl.Result{}, nil
		}
		// Nova has it but we don't: create it from Nova's view.
		return ctrl.Result{}, c.createFromServer(ctx, log, req.Name, server)
	default:
		return ctrl.Result{}, fmt.Errorf("getting VirtualMachine %s: %w", req.Name, getErr)
	}
}

// syncExisting applies Nova's view onto an existing resource, records the
// NovaSynced condition, and persists whatever changed. When the server is gone
// (found == false) the Nova-derived fields are cleared instead.
func (c *Controller) syncExisting(ctx context.Context, log logr.Logger, vm *v1alpha1.VirtualMachine, server *servers.Server, found bool) error {
	base := vm.DeepCopy()

	applyNovaState(vm, server) // server may be nil, which clears the fields.

	reason, message := v1alpha1.ConditionReasonNovaSyncedUpToDate, "resource reflects the current nova state"
	if !found {
		reason, message = v1alpha1.ConditionReasonNovaSyncedGone, "nova no longer knows this server; nova fields cleared"
	}
	meta.SetStatusCondition(&vm.Status.Conditions, novaSyncedCondition(metav1.ConditionTrue, reason, message))

	if novaStateEqual(base, vm) && !conditionChanged(base, vm) {
		// Nothing meaningful changed; skip the write (and the churn of bumping
		// LastSyncTime every interval).
		return nil
	}
	stampSyncTime(vm)

	if err := c.persist(ctx, base, vm); err != nil {
		return err
	}
	log.V(1).Info("synced VirtualMachine with nova", "name", vm.Name, "serverFound", found)
	return nil
}

// createFromServer creates a new resource carrying the fields Nova provides,
// then records the sync outcome on its status.
func (c *Controller) createFromServer(ctx context.Context, log logr.Logger, uuid string, server *servers.Server) error {
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: uuid}}
	applyNovaState(vm, server)

	if err := c.Create(ctx, vm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // Raced with another create; the next reconcile syncs it.
		}
		return fmt.Errorf("creating VirtualMachine %s: %w", uuid, err)
	}

	// Create only persists spec + metadata, so record status separately.
	meta.SetStatusCondition(&vm.Status.Conditions, novaSyncedCondition(
		metav1.ConditionTrue, v1alpha1.ConditionReasonNovaSyncedUpToDate, "resource created from nova state"))
	stampSyncTime(vm)
	if err := c.Status().Update(ctx, vm); err != nil {
		return fmt.Errorf("updating created VirtualMachine %s status: %w", uuid, err)
	}
	log.V(1).Info("created VirtualMachine from nova", "name", uuid)
	return nil
}

// persist writes the difference between base and vm back to the apiserver.
// Spec and status are separate subresources, so each is patched independently;
// a status patch ignores spec changes and vice versa. The spec patch is applied
// first because it advances the resourceVersion the status patch then builds on.
func (c *Controller) persist(ctx context.Context, base, vm *v1alpha1.VirtualMachine) error {
	desiredStatus := vm.Status

	if specChanged(base, vm) {
		if err := c.Patch(ctx, vm, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("patching VirtualMachine %s spec: %w", vm.Name, err)
		}
		// The spec patch replaced vm (incl. its status) with the stored object;
		// restore the status we intend to write.
		vm.Status = desiredStatus
	}

	statusBase := base.DeepCopy()
	statusBase.ObjectMeta = vm.ObjectMeta // match the current resourceVersion
	if err := c.Status().Patch(ctx, vm, client.MergeFrom(statusBase)); err != nil {
		return fmt.Errorf("patching VirtualMachine %s status: %w", vm.Name, err)
	}
	return nil
}

// markSyncFailed records a NovaSynced=False condition on the resource when a
// Nova fetch fails. Best-effort: errors here are logged, not propagated, since
// the caller already returns the underlying fetch error for retry.
func (c *Controller) markSyncFailed(ctx context.Context, key client.ObjectKey, cause error) {
	log := logf.FromContext(ctx)

	var vm v1alpha1.VirtualMachine
	if err := c.Get(ctx, key, &vm); err != nil {
		return // Resource may not exist; nothing to annotate.
	}
	base := vm.DeepCopy()
	meta.SetStatusCondition(&vm.Status.Conditions, novaSyncedCondition(
		metav1.ConditionFalse, v1alpha1.ConditionReasonNovaSyncedError, cause.Error()))
	if err := c.Status().Patch(ctx, &vm, client.MergeFrom(base)); err != nil {
		log.V(1).Info("could not record nova sync failure", "name", key.Name, "error", err.Error())
	}
}

// novaSyncedCondition builds a NovaSynced condition with the given outcome.
func novaSyncedCondition(status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:    v1alpha1.ConditionTypeNovaSynced,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
}

// conditionChanged reports whether the NovaSynced condition differs between base
// and vm (ignoring the condition's own timestamps, which SetStatusCondition only
// bumps on a real change).
func conditionChanged(base, vm *v1alpha1.VirtualMachine) bool {
	old := meta.FindStatusCondition(base.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	cur := meta.FindStatusCondition(vm.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	if old == nil || cur == nil {
		return old != cur
	}
	return old.Status != cur.Status || old.Reason != cur.Reason || old.Message != cur.Message
}

// stampSyncTime records the current time as the last successful Nova sync.
func stampSyncTime(vm *v1alpha1.VirtualMachine) {
	if vm.Status.Nova == nil {
		vm.Status.Nova = &v1alpha1.NovaStatus{}
	}
	vm.Status.Nova.LastSyncTime = new(metav1.Now())
}

// Start runs the periodic poll loop until ctx is cancelled. It implements
// manager.Runnable so the manager owns its lifecycle; it is registered via
// mgr.Add in SetupWithManager.
func (c *Controller) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("nova-poll")

	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	// Safety: ensure interval is at least MinInterval to prevent tight loops.
	if interval < MinInterval {
		log.Info("poll interval too low, using minimum",
			"configuredInterval", c.Interval, "minInterval", MinInterval)
		interval = MinInterval
	}

	log.Info("starting nova poll loop", "interval", interval)

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

// poll decides which resources need reconciling and enqueues their uuids. It
// lists Nova once and lists the VM CRDs once, then enqueues a uuid when the two
// disagree on any Nova-derived field (or when one side has a server/resource the
// other lacks). It performs no per-server Nova calls — that is Reconcile's job.
// Errors are logged rather than returned so a transient failure does not tear
// down the loop.
func (c *Controller) poll(ctx context.Context, log logr.Logger) {
	novaServers, err := c.OpenStack.FetchServers(ctx)
	if err != nil {
		log.Error(err, "failed to list servers from nova")
		return
	}

	var vmList v1alpha1.VirtualMachineList
	if err := c.List(ctx, &vmList); err != nil {
		log.Error(err, "failed to list VirtualMachines")
		return
	}

	uuids := uuidsToReconcile(novaServers, vmList.Items)
	log.V(1).Info("computed reconcile set",
		"servers", len(novaServers), "vms", len(vmList.Items), "toReconcile", len(uuids))

	for _, uuid := range uuids {
		// The VM CRD is named after the Nova server uuid, so an event carrying a
		// VirtualMachine with that name is translated by EnqueueRequestForObject
		// into the right reconcile request.
		evt := event.GenericEvent{Object: &v1alpha1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: uuid},
		}}
		select {
		case c.eventCh <- evt:
		case <-ctx.Done():
			return
		}
	}
}

// uuidsToReconcile returns, in stable order, the uuids whose VM CRD does not
// match the Nova server listing and therefore need a reconcile:
//
//   - a Nova server with no CRD (Reconcile will create it),
//   - a CRD with no Nova server (Reconcile will clear its Nova fields), and
//   - a CRD whose Nova-derived fields differ from the server (Reconcile will
//     sync it).
//
// uuids already in sync are omitted so steady-state polls enqueue nothing.
func uuidsToReconcile(novaServers []servers.Server, vms []v1alpha1.VirtualMachine) []string {
	byUUID := make(map[string]*servers.Server, len(novaServers))
	for i := range novaServers {
		byUUID[novaServers[i].ID] = &novaServers[i]
	}
	hasVM := make(map[string]struct{}, len(vms))

	var out []string
	for i := range vms {
		vm := &vms[i]
		hasVM[vm.Name] = struct{}{}

		switch server, inNova := byUUID[vm.Name]; {
		case !inNova && novaFieldsSet(vm):
			// Server gone but the CRD still carries its Nova fields.
			out = append(out, vm.Name)
		case inNova && novaStateDiffers(vm, server):
			out = append(out, vm.Name)
		}
	}

	// Nova servers that have no CRD yet.
	for i := range novaServers {
		if _, ok := hasVM[novaServers[i].ID]; !ok {
			out = append(out, novaServers[i].ID)
		}
	}

	sort.Strings(out)
	return out
}

// SetupWithManager wires the periodic poll loop into the manager and registers
// the reconciler to consume the uuids it enqueues.
//
// There is no For(&VirtualMachine{}) watch: this controller is driven solely by
// the eventCh raw source fed by the poll loop. Watch-driven reconciliation of
// the CRD (e.g. pruning) is handled by CleanupController.
func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	if mgr == nil {
		return errors.New("nil manager")
	}

	c.eventCh = make(chan event.GenericEvent)
	src := source.Channel(c.eventCh, &handler.EnqueueRequestForObject{})

	// Register the poll loop so the manager starts and stops it alongside the
	// controller.
	if err := mgr.Add(c); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("nova").
		WatchesRawSource(src).
		Complete(c)
}
