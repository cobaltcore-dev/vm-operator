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
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"k8s.io/apimachinery/pkg/api/equality"
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

// MinInterval clamps the poll interval to avoid a tight loop that hammers Nova.
const MinInterval = time.Second

// DefaultInterval is the poll interval used when none (or a non-positive one) is
// configured; it is also the --nova-sync-interval flag default (see cmd/main.go).
const DefaultInterval = 5 * time.Minute

// Controller keeps VirtualMachine resources in sync with the Nova servers they
// mirror. A periodic poll loop (Start) decides *which* resources are out of sync
// and enqueues their uuids; Reconcile does the per-resource work, fetching one
// server's state from Nova and writing it onto the resource. The poll loop makes
// no per-server Nova calls — it diffs a single bulk listing.
//
// Reconcile never deletes resources; pruning is GarbageCollector's job. It only
// records Nova's view, including clearing the Nova fields when the server is gone.
//
// RBAC (templated by hand in dist/templates/rbac.yaml):
//   - cobaltcore.cloud/virtualmachines: get;list;watch;create;update;patch;delete
//   - cobaltcore.cloud/virtualmachines/status: get;update;patch
//   - cobaltcore.cloud/virtualmachines/finalizers: update
type Controller struct {
	client.Client
	Scheme *runtime.Scheme

	// OpenStack is the Nova API the poll loop and Reconcile talk to.
	OpenStack OpenStackClient

	// Region (from OS_REGION_NAME) is written to Spec.Region of created VMs.
	Region string

	// Interval is how often the poll loop lists Nova. Clamped to [MinInterval, ∞);
	// a non-positive value falls back to DefaultInterval.
	Interval time.Duration

	// eventCh carries the uuids the poll loop decides to reconcile; created in
	// SetupWithManager, closed by Start on shutdown.
	eventCh chan event.GenericEvent

	// Metrics records reconcile and poll instrumentation. May be nil (no-op).
	Metrics *Metrics
}

// Reconcile brings a single VirtualMachine in line with Nova's view of the
// server with the same uuid. It fetches the server (and its flavor), converts
// them, and dispatches on whether each side exists:
//
//	           │ in Nova              │ not in Nova
//	───────────┼──────────────────────┼─────────────────────
//	 in k8s    │ unchanged: noop      │ clear the Nova fields
//	           │ changed: patch       │
//	───────────┼──────────────────────┼─────────────────────
//	 not in k8s│ create (may skip)    │ noop
//
// On success it records NovaSynced=True; on a fetch error, NovaSynced=False, and
// returns the error so the request is retried.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	start := time.Now()

	// Fetch the server. A fetch error is retried (and recorded on the resource);
	// a gone server (inNova == false) means Nova no longer knows it.
	server, inNova, err := c.OpenStack.FetchServer(ctx, req.Name)
	if err != nil {
		err = fmt.Errorf("fetching nova server %s: %w", req.Name, err)
		// Best-effort: record NovaSynced=False on the resource if it exists, then
		// retry (the error is still returned to requeue).
		var vm v1alpha1.VirtualMachine
		if c.Get(ctx, req.NamespacedName, &vm) == nil {
			base := vm.DeepCopy()
			meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
				Type:    v1alpha1.ConditionTypeNovaSynced,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ConditionReasonNovaSyncedError,
				Message: err.Error(),
			})
			if perr := c.Status().Patch(ctx, &vm, client.MergeFrom(base)); perr != nil {
				log.Error(perr, "could not record nova sync failure on resource", "name", req.Name)
			}
		}
		c.Metrics.observeReconcile(outcomeError, start)
		return ctrl.Result{}, err
	}

	var vm v1alpha1.VirtualMachine
	getErr := c.Get(ctx, req.NamespacedName, &vm)
	inK8s := getErr == nil
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		c.Metrics.observeReconcile(outcomeError, start)
		return ctrl.Result{}, fmt.Errorf("getting VirtualMachine %s: %w", req.Name, getErr)
	}

	// Case: neither side has it — nothing to do.
	if !inK8s && !inNova {
		c.Metrics.observeReconcile(outcomeNoop, start)
		return ctrl.Result{}, nil
	}

	// Convert a server in Nova to the VirtualMachine it maps onto. Every server
	// must have a findable, managed flavor; if the id is absent, the flavor cannot
	// be fetched, or its hypervisor type is unmanaged, the server is skipped (a
	// noop, not an error — the next poll re-enqueues if its state changes). A gone
	// server needs no conversion.
	var conv ConvertedVirtualMachine
	if inNova {
		// Resolve the flavor by its id (a missing id or a failed fetch leaves it nil,
		// which convert rejects as unmanaged). An unmanaged hypervisor type is an
		// expected skip (noop, not an error — the next poll re-enqueues it); any other
		// conversion error is unexpected and propagated for retry.
		var flavor *flavors.Flavor
		if id, _ := server.Flavor["id"].(string); id != "" {
			if f, ferr := c.OpenStack.FetchFlavor(ctx, id); ferr == nil {
				flavor = f
			}
		}
		converted, convErr := ConvertToVirtualMachine(server, flavor, c.Region)
		switch {
		case errors.Is(convErr, ErrUnmanagedHypervisorType):
			log.V(1).Info("skipping: nova server has no managed flavor", "name", req.Name)
			c.Metrics.observeReconcile(outcomeNoop, start)
			return ctrl.Result{}, nil
		case convErr != nil:
			c.Metrics.observeReconcile(outcomeError, start)
			return ctrl.Result{}, fmt.Errorf("converting nova server %s: %w", req.Name, convErr)
		}
		conv = converted
	}

	// Case: Nova has the server but k8s does not — adopt it, unless it is not yet
	// schedulable (no availability zone), which is a noop rather than a create.
	if !inK8s {
		if conv.Spec.AZ == "" {
			log.V(1).Info("skipping create: nova server has no availability zone yet", "name", req.Name)
			c.Metrics.observeReconcile(outcomeNoop, start)
			return ctrl.Result{}, nil
		}
		created := conv.DeepCopy() // ObjectMeta.Name is already the server uuid.
		if err := c.Create(ctx, created); err != nil {
			if apierrors.IsAlreadyExists(err) {
				c.Metrics.observeReconcile(outcomeNoop, start) // Raced; the next reconcile syncs it.
				return ctrl.Result{}, nil
			}
			c.Metrics.observeReconcile(outcomeError, start)
			return ctrl.Result{}, fmt.Errorf("creating VirtualMachine %s: %w", req.Name, err)
		}
		// Create persists spec + metadata only and clears the status subresource, so
		// re-establish the Nova status (from the conversion) and record it separately.
		created.Status.Nova = &v1alpha1.NovaStatus{
			Status:       conv.Status.Nova.Status,
			Host:         conv.Status.Nova.Host,
			LastSyncTime: new(metav1.Now()),
		}
		meta.SetStatusCondition(&created.Status.Conditions, metav1.Condition{
			Type:    v1alpha1.ConditionTypeNovaSynced,
			Status:  metav1.ConditionTrue,
			Reason:  v1alpha1.ConditionReasonNovaSyncedUpToDate,
			Message: "resource created from nova state",
		})
		if err := c.Status().Update(ctx, created); err != nil {
			c.Metrics.observeReconcile(outcomeError, start)
			return ctrl.Result{}, fmt.Errorf("updating created VirtualMachine %s status: %w", req.Name, err)
		}
		// Adoption is a meaningful, low-frequency transition; log at Info (syncs stay V(1)).
		log.Info("created VirtualMachine from nova", "name", req.Name)
		c.Metrics.observeReconcile(outcomeCreated, start)
		return ctrl.Result{}, nil
	}

	// Case: k8s has the resource — patch Nova's view onto it (inNova), or clear the
	// Nova fields when the server is gone. LastSyncTime, conditions and Status.Host
	// are preserved either way.
	base := vm.DeepCopy()
	reason, message := v1alpha1.ConditionReasonNovaSyncedUpToDate, "resource reflects the current nova state"
	outcome := outcomeSynced
	var changed bool
	if inNova {
		changed = conv.HasChanged(&vm)
		conv.PatchInto(&vm)
	} else {
		var lastSync *metav1.Time
		var status string
		if vm.Status.Nova != nil {
			lastSync = vm.Status.Nova.LastSyncTime
			status = vm.Status.Nova.Status
		}
		changed = !equality.Semantic.DeepEqual(vm.Spec.Nova, v1alpha1.NovaSpec{}) || status != ""
		vm.Spec.Nova = v1alpha1.NovaSpec{}
		vm.Status.Nova = &v1alpha1.NovaStatus{LastSyncTime: lastSync}
		reason, message = v1alpha1.ConditionReasonNovaSyncedGone, "nova no longer knows this server; nova fields cleared"
		outcome = outcomeCleared
	}
	meta.SetStatusCondition(&vm.Status.Conditions, metav1.Condition{
		Type:    v1alpha1.ConditionTypeNovaSynced,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: message,
	})

	// Skip the write when nothing meaningful changed: neither the Nova fields nor
	// the NovaSynced condition (ignoring the timestamp SetStatusCondition only
	// bumps on a real change).
	oldCond := meta.FindStatusCondition(base.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	newCond := meta.FindStatusCondition(vm.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	conditionChanged := (oldCond == nil) != (newCond == nil) ||
		(oldCond != nil && newCond != nil &&
			(oldCond.Status != newCond.Status || oldCond.Reason != newCond.Reason || oldCond.Message != newCond.Message))
	if !changed && !conditionChanged {
		c.Metrics.observeReconcile(outcome, start)
		return ctrl.Result{}, nil
	}
	if vm.Status.Nova == nil {
		vm.Status.Nova = &v1alpha1.NovaStatus{}
	}
	vm.Status.Nova.LastSyncTime = new(metav1.Now())

	// Spec and status are separate subresources, patched independently; the spec
	// patch goes first because it advances the resourceVersion the status patch
	// then builds on.
	desiredStatus := vm.Status
	if !equality.Semantic.DeepEqual(base.Spec, vm.Spec) {
		if err := c.Patch(ctx, &vm, client.MergeFrom(base)); err != nil {
			c.Metrics.observeReconcile(outcomeError, start)
			return ctrl.Result{}, fmt.Errorf("patching VirtualMachine %s spec: %w", req.Name, err)
		}
		// The spec patch replaced vm (incl. status) with the stored object; restore ours.
		vm.Status = desiredStatus
	}
	statusBase := base.DeepCopy()
	statusBase.ObjectMeta = vm.ObjectMeta // match the current resourceVersion
	if err := c.Status().Patch(ctx, &vm, client.MergeFrom(statusBase)); err != nil {
		c.Metrics.observeReconcile(outcomeError, start)
		return ctrl.Result{}, fmt.Errorf("patching VirtualMachine %s status: %w", req.Name, err)
	}
	log.V(1).Info("synced VirtualMachine with nova", "name", req.Name, "serverFound", inNova)
	c.Metrics.observeReconcile(outcome, start)
	return ctrl.Result{}, nil
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

// poll converts every managed Nova server, diffs each against its k8s
// counterpart, and enqueues the uuids that need reconciling: a managed server
// with no CRD (create), a CRD whose fields differ (sync), or a CRD whose server
// is gone but still carries Nova fields (clear). It makes no per-server Nova
// calls — that is Reconcile's job — and logs errors rather than returning them so
// a transient failure does not tear down the loop.
func (c *Controller) poll(ctx context.Context, log logr.Logger) {
	start := time.Now()

	novaServers, err := c.OpenStack.FetchServers(ctx)
	if err != nil {
		log.Error(err, "failed to list servers from nova")
		c.Metrics.recordPollError("fetch_servers")
		c.Metrics.observePoll("error", start)
		return
	}
	flavorsByID, err := c.OpenStack.FetchFlavors(ctx)
	if err != nil {
		log.Error(err, "failed to list flavors from nova")
		c.Metrics.recordPollError("fetch_flavors")
		c.Metrics.observePoll("error", start)
		return
	}
	var vmList v1alpha1.VirtualMachineList
	if err := c.List(ctx, &vmList); err != nil {
		log.Error(err, "failed to list VirtualMachines")
		c.Metrics.recordPollError("list_vms")
		c.Metrics.observePoll("error", start)
		return
	}

	// Convert every server, keeping the managed ones (qemu/ch) keyed by uuid. An
	// unmanaged hypervisor type is the expected skip; any other conversion error is
	// unexpected, so log it (the server is still skipped) rather than swallow it.
	managed := make(map[string]ConvertedVirtualMachine, len(novaServers))
	for i := range novaServers {
		flavorID, _ := novaServers[i].Flavor["id"].(string) // comma-ok: may be absent.
		conv, err := ConvertToVirtualMachine(&novaServers[i], flavorsByID[flavorID], c.Region)
		switch {
		case errors.Is(err, ErrUnmanagedHypervisorType):
			continue
		case err != nil:
			log.Error(err, "skipping nova server: unexpected conversion error", "name", novaServers[i].ID)
			continue
		}
		managed[novaServers[i].ID] = conv
	}

	// A CRD with no managed server needs reconciling regardless (freshly created in
	// k8s, or vanished from Nova and awaiting a clear); otherwise enqueue only when
	// its server differs. Then add managed servers that have no CRD yet.
	hasVM := make(map[string]struct{}, len(vmList.Items))
	var uuids []string
	for i := range vmList.Items {
		vm := &vmList.Items[i]
		hasVM[vm.Name] = struct{}{}
		if conv, inNova := managed[vm.Name]; !inNova || conv.HasChanged(vm) {
			uuids = append(uuids, vm.Name)
		}
	}
	for uuid := range managed {
		if _, ok := hasVM[uuid]; !ok {
			uuids = append(uuids, uuid)
		}
	}
	sort.Strings(uuids)

	// Record gauges + duration now: the fallible work is done, and the enqueue
	// loop below can return early on ctx cancellation.
	c.Metrics.setPollGauges(len(managed), len(vmList.Items), len(uuids))
	c.Metrics.observePoll("success", start)
	log.V(1).Info("computed reconcile set",
		"servers", len(novaServers), "managedServers", len(managed),
		"vms", len(vmList.Items), "toReconcile", len(uuids))

	for _, uuid := range uuids {
		// The CRD is named after the server uuid, so EnqueueRequestForObject turns
		// this event into the right reconcile request.
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

// SetupWithManager wires the periodic poll loop into the manager and registers
// the reconciler to consume the uuids it enqueues.
//
// There is no For(&VirtualMachine{}) watch: this controller is driven solely by
// the eventCh raw source fed by the poll loop. Watch-driven reconciliation of
// the CRD (e.g. pruning) is handled by GarbageCollector.
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
