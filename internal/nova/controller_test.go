// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// fakeOpenStackClient is a stub OpenStackClient that returns canned results
// from FetchChangedServers and records how often it was called.
type fakeOpenStackClient struct {
	servers []string
	err     error
	calls   int
}

func (f *fakeOpenStackClient) FetchChangedServers() ([]string, error) {
	f.calls++
	return f.servers, f.err
}

// newTestReconciler builds a Controller backed by a fake client seeded with
// the given objects.
func newTestReconciler(t *testing.T, objs ...client.Object) *Controller {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	return &Controller{Client: c, Scheme: scheme}
}

func TestController_Reconcile(t *testing.T) {
	const vmName = "63ed64e2-27a2-4b70-9b5c-833c2a285d66"

	existingVM := &v1alpha1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: vmName},
		Spec: v1alpha1.VMSpec{
			Region: "qa-de-1",
			AZ:     "qa-de-1a",
		},
	}

	tests := []struct {
		name    string
		objects []client.Object
		request ctrl.Request
	}{
		{
			name:    "existing VM reconciles without error",
			objects: []client.Object{existingVM},
			request: ctrl.Request{NamespacedName: types.NamespacedName{Name: vmName}},
		},
		{
			// The VM was deleted after the request was enqueued; the reconciler
			// must tolerate the not-found and not error.
			name:    "missing VM is tolerated",
			objects: nil,
			request: ctrl.Request{NamespacedName: types.NamespacedName{Name: "does-not-exist"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReconciler(t, tc.objects...)

			result, err := r.Reconcile(context.Background(), tc.request)
			if err != nil {
				t.Fatalf("Reconcile returned unexpected error: %v", err)
			}

			// The controller is a no-op skeleton: it never requeues.
			if result != (ctrl.Result{}) {
				t.Errorf("expected empty result, got %+v", result)
			}
		})
	}
}

func TestController_SetupWithManager_NilManager(t *testing.T) {
	// SetupWithManager must fail rather than panic when given no manager. This
	// guards the wiring called from cmd/main.go.
	c := &Controller{}
	if err := c.SetupWithManager(nil); err == nil {
		t.Fatal("expected SetupWithManager(nil) to return an error, got nil")
	}
}

func TestController_poll_enqueuesChangedServers(t *testing.T) {
	changed := []string{
		"63ed64e2-27a2-4b70-9b5c-833c2a285d66",
		"a1b2c3d4-0000-1111-2222-333344445555",
	}
	c := &Controller{
		OpenStack: &fakeOpenStackClient{servers: changed},
		// Buffer the channel so poll can enqueue without a concurrent reader.
		eventCh: make(chan event.GenericEvent, len(changed)),
	}

	c.poll(context.Background(), logr.Discard())

	// Each changed server uuid must surface as a GenericEvent carrying a
	// VirtualMachine named after that uuid, so EnqueueRequestForObject
	// produces the matching reconcile request.
	for _, want := range changed {
		select {
		case evt := <-c.eventCh:
			if got := evt.Object.GetName(); got != want {
				t.Errorf("enqueued event for %q, want %q", got, want)
			}
		default:
			t.Fatalf("expected an event for %q, channel was empty", want)
		}
	}

	select {
	case evt := <-c.eventCh:
		t.Fatalf("unexpected extra event enqueued: %q", evt.Object.GetName())
	default:
	}
}

func TestController_poll_toleratesFetchError(t *testing.T) {
	stub := &fakeOpenStackClient{err: errors.New("nova unreachable")}
	c := &Controller{
		OpenStack: stub,
		eventCh:   make(chan event.GenericEvent, 1),
	}

	// poll must log and return rather than panic or block; nothing is enqueued.
	c.poll(context.Background(), logr.Discard())

	if stub.calls != 1 {
		t.Errorf("FetchChangedServers called %d times, want 1", stub.calls)
	}
	select {
	case evt := <-c.eventCh:
		t.Fatalf("expected no events on fetch error, got %q", evt.Object.GetName())
	default:
	}
}

func TestController_poll_respectsContextCancellation(t *testing.T) {
	// With an unbuffered channel and no reader, poll must unblock when the
	// context is cancelled rather than deadlocking on the channel send.
	c := &Controller{
		OpenStack: &fakeOpenStackClient{servers: []string{"stuck-uuid"}},
		eventCh:   make(chan event.GenericEvent),
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
