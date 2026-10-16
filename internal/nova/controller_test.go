// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// testVMUUID is the Nova server uuid / VirtualMachine name used across the
// controller and cleanup tests.
const testVMUUID = "63ed64e2-27a2-4b70-9b5c-833c2a285d66"

// seedVM builds a VirtualMachine the way the controller's sync path would, by
// converting server with a managed flavor and patching the Nova fields in. Tests
// use it to construct the expected in-sync state for a given server.
func seedVM(name string, server *servers.Server) *v1alpha1.VirtualMachine {
	conv, err := ConvertToVirtualMachine(server, qemuFlavor("f", 1, 1), "")
	if err != nil {
		panic(err) // qemuFlavor is always managed.
	}
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	conv.PatchInto(vm)
	return vm
}

// fakeOpenStackClient is a stub OpenStackClient. FetchServers returns the bulk
// listing; FetchServer looks a single server up in the same slice (or honours a
// per-uuid override), so tests can drive the poll loop and Reconcile from one
// fixture.
type fakeOpenStackClient struct {
	result []servers.Server // bulk listing and single-get source
	err    error            // error returned by both methods
	calls  int              // FetchServers call count

	getErr map[string]error // optional per-uuid FetchServer errors

	flavors   map[string]*flavors.Flavor // flavors keyed by id, for FetchFlavor/FetchFlavors
	flavorErr error                      // optional FetchFlavor/FetchFlavors error
}

func (f *fakeOpenStackClient) FetchServers(context.Context) ([]servers.Server, error) {
	f.calls++
	return f.result, f.err
}

func (f *fakeOpenStackClient) FetchServer(_ context.Context, uuid string) (*servers.Server, bool, error) {
	if f.err != nil {
		return nil, false, f.err
	}
	if err := f.getErr[uuid]; err != nil {
		return nil, false, err
	}
	for i := range f.result {
		if f.result[i].ID == uuid {
			return &f.result[i], true, nil
		}
	}
	return nil, false, nil
}

func (f *fakeOpenStackClient) FetchFlavor(_ context.Context, id string) (*flavors.Flavor, error) {
	if f.flavorErr != nil {
		return nil, f.flavorErr
	}
	return f.flavors[id], nil
}

func (f *fakeOpenStackClient) FetchFlavors(context.Context) (map[string]*flavors.Flavor, error) {
	if f.flavorErr != nil {
		return nil, f.flavorErr
	}
	return f.flavors, nil
}

// qemuFlavor is a flavor fixture whose extra specs mark it as a managed (qemu)
// hypervisor type, with the given id (also used as its name), vCPUs, and RAM (MB).
func qemuFlavor(id string, vcpus, ramMB int) *flavors.Flavor {
	return &flavors.Flavor{
		ID:         id,
		Name:       id,
		VCPUs:      vcpus,
		RAM:        ramMB,
		ExtraSpecs: map[string]string{hypervisorTypeExtraSpec: "qemu"},
	}
}

// newTestScheme builds a runtime scheme with the VM CRD registered.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}
	return scheme
}

// newTestClient builds a fake client seeded with the given objects, with the
// VirtualMachine status subresource enabled so Status().Patch/Update works.
func newTestClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.VirtualMachine{}).
		Build()
}

// drainEvents reads every uuid currently buffered on the channel.
func drainEvents(ch chan event.GenericEvent) map[string]bool {
	got := map[string]bool{}
	for {
		select {
		case evt := <-ch:
			got[evt.Object.GetName()] = true
		default:
			return got
		}
	}
}

func TestController_SetupWithManager_NilManager(t *testing.T) {
	// SetupWithManager must fail rather than panic when given no manager.
	if err := (&Controller{}).SetupWithManager(nil); err == nil {
		t.Fatal("expected SetupWithManager(nil) to return an error, got nil")
	}
}

func TestController_poll_enqueuesOutOfSyncUUIDs(t *testing.T) {
	const (
		inSync    = "11111111-1111-1111-1111-111111111111"
		outOfSync = "22222222-2222-2222-2222-222222222222"
		novaOnly  = "33333333-3333-3333-3333-333333333333"
		orphan    = "44444444-4444-4444-4444-444444444444"
	)

	scheme := newTestScheme(t)

	flavorRef := map[string]any{"id": "f"}
	// inSync: CRD already reflects the server.
	inSyncVM := seedVM(inSync, &servers.Server{ID: inSync, Status: "ACTIVE", Host: "compute-1", Flavor: flavorRef})
	// outOfSync: CRD status differs from the server.
	outVM := seedVM(outOfSync, &servers.Server{ID: outOfSync, Status: "ACTIVE", Host: "compute-1", Flavor: flavorRef})
	// orphan: CRD carries Nova fields but the server is gone.
	orphanVM := seedVM(orphan, &servers.Server{ID: orphan, Status: "ACTIVE", Host: "compute-9", Flavor: flavorRef})

	c := &Controller{
		Client: newTestClient(t, scheme, inSyncVM, outVM, orphanVM),
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{
				{ID: inSync, Status: "ACTIVE", Host: "compute-1", Flavor: flavorRef},     // unchanged
				{ID: outOfSync, Status: "SHUTOFF", Host: "compute-1", Flavor: flavorRef}, // status changed
				{ID: novaOnly, Status: "ACTIVE", Host: "compute-2", Flavor: flavorRef},   // no CRD yet
				// orphan absent from Nova
			},
			flavors: map[string]*flavors.Flavor{"f": qemuFlavor("f", 2, 2048)},
		},
		eventCh: make(chan event.GenericEvent, 8),
	}

	c.poll(context.Background(), logr.Discard())
	got := drainEvents(c.eventCh)

	for _, want := range []string{outOfSync, novaOnly, orphan} {
		if !got[want] {
			t.Errorf("expected %q to be enqueued, got %v", want, got)
		}
	}
	if got[inSync] {
		t.Errorf("in-sync uuid should not be enqueued, got %v", got)
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 enqueued uuids, got %d: %v", len(got), got)
	}
}

func TestController_poll_toleratesFetchError(t *testing.T) {
	stub := &fakeOpenStackClient{err: errors.New("nova unreachable")}
	c := &Controller{
		Client:    newTestClient(t, newTestScheme(t)),
		OpenStack: stub,
		eventCh:   make(chan event.GenericEvent, 1),
	}

	c.poll(context.Background(), logr.Discard())

	if stub.calls != 1 {
		t.Errorf("FetchServers called %d times, want 1", stub.calls)
	}
	if got := drainEvents(c.eventCh); len(got) != 0 {
		t.Errorf("expected no events on fetch error, got %v", got)
	}
}

func TestController_poll_respectsContextCancellation(t *testing.T) {
	// With an unbuffered channel and no reader, poll must unblock when the
	// context is cancelled rather than deadlocking on the channel send.
	c := &Controller{
		Client: newTestClient(t, newTestScheme(t)),
		OpenStack: &fakeOpenStackClient{
			result:  []servers.Server{{ID: "stuck-uuid", Status: "ACTIVE", Flavor: map[string]any{"id": "f"}}},
			flavors: map[string]*flavors.Flavor{"f": qemuFlavor("f", 2, 2048)},
		},
		eventCh: make(chan event.GenericEvent),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		c.poll(ctx, logr.Discard())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poll did not return after context cancellation")
	}
}

func TestController_Reconcile_syncsExistingVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c := &Controller{
		Client: newTestClient(t, scheme, vm),
		Scheme: scheme,
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{
				{ID: name, Status: "ACTIVE", Host: "compute-1", TenantID: "proj", InstanceName: "web-1", Flavor: map[string]any{"id": "f"}},
			},
			flavors: map[string]*flavors.Flavor{"f": qemuFlavor("f", 2, 2048)},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getVM(t, c)
	if got.Status.Nova == nil || got.Status.Nova.Status != "ACTIVE" {
		t.Errorf("status.nova = %+v, want ACTIVE", got.Status.Nova)
	}
	// Status.Host is not owned by the nova syncer (it reflects the actual runtime
	// host, derived elsewhere from libvirt/cloud-hypervisor), so it must stay unset.
	if got.Status.Host != nil {
		t.Errorf("status.host = %+v, want nil (not set by the nova syncer)", got.Status.Host)
	}
	if got.Spec.Nova.InstanceName != "web-1" || got.Spec.Nova.Ownership.Project != "proj" {
		t.Errorf("spec.nova = %+v, want instanceName web-1 / project proj", got.Spec.Nova)
	}
	if got.Status.Nova.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be stamped on sync")
	}
	assertSyncedCondition(t, &got, metav1.ConditionTrue, v1alpha1.ConditionReasonNovaSyncedUpToDate)
}

func TestController_Reconcile_syncsFlavorOntoExistingVM(t *testing.T) {
	const name = testVMUUID

	// An existing VM carrying no flavor must get it patched in on sync, from the
	// flavor Reconcile fetches alongside the server.
	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c := &Controller{
		Client: newTestClient(t, scheme, vm),
		Scheme: scheme,
		OpenStack: &fakeOpenStackClient{
			result:  []servers.Server{{ID: name, Status: "ACTIVE", Flavor: map[string]any{"id": "flavor-uuid"}}},
			flavors: map[string]*flavors.Flavor{"flavor-uuid": qemuFlavor("flavor-uuid", 4, 8192)},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getVM(t, c)
	if got.Spec.Nova.Flavor.Name != "flavor-uuid" || got.Spec.Nova.Flavor.FlavorID != "flavor-uuid" {
		t.Errorf("spec.nova.flavor = %+v, want name/id flavor-uuid", got.Spec.Nova.Flavor)
	}
}

func TestController_Reconcile_createsMissingVM(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	c := &Controller{
		Client: newTestClient(t, scheme), // no CRD seeded
		Scheme: scheme,
		Region: "qa-de-1",
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{{
				ID:               name,
				Status:           "BUILD",
				AvailabilityZone: "qa-de-1a",
				Host:             "compute-1",
				Flavor:           map[string]any{"id": "flavor-uuid"},
			}},
			flavors: map[string]*flavors.Flavor{"flavor-uuid": qemuFlavor("flavor-uuid", 4, 8192)},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getVM(t, c)
	if got.Status.Nova == nil || got.Status.Nova.Status != "BUILD" {
		t.Errorf("created VM status.nova = %+v, want BUILD", got.Status.Nova)
	}
	// Required spec fields must be derived so the Create passes CRD validation.
	if got.Spec.AZ != "qa-de-1a" {
		t.Errorf("spec.az = %q, want qa-de-1a", got.Spec.AZ)
	}
	if got.Spec.Region != "qa-de-1" {
		t.Errorf("spec.region = %q, want qa-de-1 (from controller config)", got.Spec.Region)
	}
	if got.Spec.HypervisorType == nil || *got.Spec.HypervisorType != v1alpha1.HypervisorTypeQEMU {
		t.Errorf("spec.hypervisorType = %v, want QEMU", got.Spec.HypervisorType)
	}
	if got.Spec.TargetHost == nil || got.Spec.TargetHost.Name != "compute-1" {
		t.Errorf("spec.targetHost = %+v, want name compute-1", got.Spec.TargetHost)
	}
	cpu := got.Spec.Resources[v1alpha1.ResourceCPU]
	if cpu.Value() != 4 {
		t.Errorf("spec.resources[cpu] = %s, want 4", cpu.String())
	}
	mem := got.Spec.Resources[v1alpha1.ResourceMemory]
	if mem.Value() != 8192*1024*1024 {
		t.Errorf("spec.resources[memory] = %s, want 8192Mi in bytes", mem.String())
	}
	// Flavor name, id, and extra specs are written from the fetched flavor.
	if got.Spec.Nova.Flavor.Name != "flavor-uuid" || got.Spec.Nova.Flavor.FlavorID != "flavor-uuid" {
		t.Errorf("spec.nova.flavor = %+v, want name/id flavor-uuid", got.Spec.Nova.Flavor)
	}
	if got.Spec.Nova.Flavor.ExtraSpecs[hypervisorTypeExtraSpec] != "qemu" {
		t.Errorf("spec.nova.flavor.extraSpecs = %+v, want hypervisor type qemu", got.Spec.Nova.Flavor.ExtraSpecs)
	}
	assertSyncedCondition(t, &got, metav1.ConditionTrue, v1alpha1.ConditionReasonNovaSyncedUpToDate)
}

func TestController_Reconcile_skipsCreateWithoutAZ(t *testing.T) {
	scheme := newTestScheme(t)
	c := &Controller{
		Client: newTestClient(t, scheme), // no CRD seeded
		Scheme: scheme,
		Region: "qa-de-1",
		// Server has no availability zone yet (e.g. not scheduled); we cannot
		// derive a valid az, so creation must be skipped (not errored).
		OpenStack: &fakeOpenStackClient{
			result:  []servers.Server{{ID: testVMUUID, Status: "BUILD", Flavor: map[string]any{"id": "flavor-uuid"}}},
			flavors: map[string]*flavors.Flavor{"flavor-uuid": qemuFlavor("flavor-uuid", 4, 8192)},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile should not error when AZ is missing, got: %v", err)
	}

	var vm v1alpha1.VirtualMachine
	err := c.Get(context.Background(), types.NamespacedName{Name: testVMUUID}, &vm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no VM to be created without an AZ, got err=%v", err)
	}
}

func TestController_Reconcile_skipsCreateForUnmanagedHypervisor(t *testing.T) {
	scheme := newTestScheme(t)
	c := &Controller{
		Client: newTestClient(t, scheme), // no CRD seeded
		Scheme: scheme,
		Region: "qa-de-1",
		// Flavor has no (managed) hypervisor type, so the server is not ours and
		// Reconcile must skip creation without erroring.
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{{
				ID:               testVMUUID,
				Status:           "ACTIVE",
				AvailabilityZone: "qa-de-1a",
				Flavor:           map[string]any{"id": "vmware-uuid"},
			}},
			flavors: map[string]*flavors.Flavor{"vmware-uuid": {
				ID:         "vmware-uuid",
				ExtraSpecs: map[string]string{hypervisorTypeExtraSpec: "vmware"},
			}},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile should not error for an unmanaged hypervisor, got: %v", err)
	}

	var vm v1alpha1.VirtualMachine
	err := c.Get(context.Background(), types.NamespacedName{Name: testVMUUID}, &vm)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no VM to be created for an unmanaged hypervisor, got err=%v", err)
	}
}

func TestController_Reconcile_returnsFlavorFetchError(t *testing.T) {
	// The poll loop already filtered this server in using its bulk flavor listing,
	// so a per-server flavor fetch failure is unexpected: Reconcile must return the
	// error (requeue) rather than silently skipping the server as "no managed flavor".
	scheme := newTestScheme(t)
	c := &Controller{
		Client: newTestClient(t, scheme), // no CRD seeded
		Scheme: scheme,
		Region: "qa-de-1",
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{{
				ID:               testVMUUID,
				Status:           "ACTIVE",
				AvailabilityZone: "qa-de-1a",
				Flavor:           map[string]any{"id": "flavor-uuid"},
			}},
			flavorErr: errors.New("flavor service unreachable"),
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err == nil {
		t.Fatal("expected Reconcile to return the flavor fetch error for retry")
	}

	var vm v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testVMUUID}, &vm); !apierrors.IsNotFound(err) {
		t.Errorf("expected no VM to be created when the flavor fetch fails, got err=%v", err)
	}
}

func TestController_Reconcile_skipsCreateWithoutFlavorID(t *testing.T) {
	// A server with no flavor id cannot be resolved to a managed flavor, so it is
	// skipped as unmanaged (a noop, not an error) — distinct from a flavor fetch
	// that fails.
	scheme := newTestScheme(t)
	c := &Controller{
		Client: newTestClient(t, scheme), // no CRD seeded
		Scheme: scheme,
		Region: "qa-de-1",
		OpenStack: &fakeOpenStackClient{
			result: []servers.Server{{
				ID:               testVMUUID,
				Status:           "ACTIVE",
				AvailabilityZone: "qa-de-1a",
				Flavor:           map[string]any{}, // no id
			}},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile should not error when the flavor id is absent, got: %v", err)
	}

	var vm v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testVMUUID}, &vm); !apierrors.IsNotFound(err) {
		t.Errorf("expected no VM to be created without a flavor id, got err=%v", err)
	}
}

func TestController_Reconcile_marksServerGone(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := seedVM(name, &servers.Server{ID: name, Status: "ACTIVE", Host: "compute-1", InstanceName: "web-1"})
	// Status.Host is owned by another component; the syncer must leave it intact.
	vm.Status.Host = &v1alpha1.HostRef{Name: "compute-1"}
	c := &Controller{
		Client:    newTestClient(t, scheme, vm),
		Scheme:    scheme,
		OpenStack: &fakeOpenStackClient{result: nil}, // server not in Nova
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getVM(t, c)
	// The last-known Nova fields must be RETAINED, not cleared: operators can still
	// see what the VM was. The gone transition is recorded only via the condition.
	if got.Spec.Nova.InstanceName != "web-1" {
		t.Errorf("expected nova spec fields retained, got spec.nova=%+v", got.Spec.Nova)
	}
	if got.Status.Nova == nil || got.Status.Nova.Status != "ACTIVE" {
		t.Errorf("expected nova status retained (ACTIVE), got status.nova=%+v", got.Status.Nova)
	}
	// Status.Host must NOT be cleared by the nova syncer.
	if got.Status.Host == nil || got.Status.Host.Name != "compute-1" {
		t.Errorf("status.host = %+v, want preserved compute-1 (not owned by syncer)", got.Status.Host)
	}
	// The resource must survive (cleanup controller prunes, not this one).
	assertSyncedCondition(t, &got, metav1.ConditionTrue, v1alpha1.ConditionReasonNovaSyncedGone)
}

func TestController_Reconcile_recordsDeletedStatus(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c := &Controller{
		Client: newTestClient(t, scheme, vm),
		Scheme: scheme,
		OpenStack: &fakeOpenStackClient{
			result:  []servers.Server{{ID: name, Status: "DELETED", Flavor: map[string]any{"id": "f"}}},
			flavors: map[string]*flavors.Flavor{"f": qemuFlavor("f", 2, 2048)},
		},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getVM(t, c)
	// A DELETED server has its status recorded as-is (not cleared), so the
	// cleanup controller can act on it.
	if got.Status.Nova == nil || got.Status.Nova.Status != "DELETED" {
		t.Errorf("status.nova = %+v, want DELETED recorded", got.Status.Nova)
	}
}

func TestController_Reconcile_recordsFetchFailure(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	c := &Controller{
		Client:    newTestClient(t, scheme, vm),
		Scheme:    scheme,
		OpenStack: &fakeOpenStackClient{getErr: map[string]error{name: errors.New("boom")}},
	}

	_, err := c.Reconcile(context.Background(), request())
	if err == nil {
		t.Fatal("expected Reconcile to return the fetch error for retry")
	}

	got := getVM(t, c)
	assertSyncedCondition(t, &got, metav1.ConditionFalse, v1alpha1.ConditionReasonNovaSyncedError)
}

func TestController_Reconcile_bothMissingIsNoop(t *testing.T) {
	const name = testVMUUID

	scheme := newTestScheme(t)
	c := &Controller{
		Client:    newTestClient(t, scheme),
		Scheme:    scheme,
		OpenStack: &fakeOpenStackClient{result: nil},
	}

	if _, err := c.Reconcile(context.Background(), request()); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	var got v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &got); !apierrors.IsNotFound(err) {
		t.Errorf("expected no resource created, got err=%v", err)
	}
}

// request builds a cluster-scoped reconcile.Request for the test VM uuid.
func request() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: testVMUUID}}
}

// getVM reads the test VirtualMachine, failing the test if absent.
func getVM(t *testing.T, c client.Reader) v1alpha1.VirtualMachine {
	t.Helper()
	var vm v1alpha1.VirtualMachine
	if err := c.Get(context.Background(), types.NamespacedName{Name: testVMUUID}, &vm); err != nil {
		t.Fatalf("get %s: %v", testVMUUID, err)
	}
	return vm
}

// assertSyncedCondition checks the NovaSynced condition's status and reason.
func assertSyncedCondition(t *testing.T, vm *v1alpha1.VirtualMachine, status metav1.ConditionStatus, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(vm.Status.Conditions, v1alpha1.ConditionTypeNovaSynced)
	if cond == nil {
		t.Fatalf("expected a %s condition, found none", v1alpha1.ConditionTypeNovaSynced)
	}
	if cond.Status != status || cond.Reason != reason {
		t.Errorf("NovaSynced condition = (%s, %s), want (%s, %s)", cond.Status, cond.Reason, status, reason)
	}
}
