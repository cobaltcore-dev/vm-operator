// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestNewMetrics_RegistersCleanly guards against duplicate or malformed metric
// names: a bad name or a collision only surfaces when the collector is
// registered, which production does exactly once against the shared
// controller-runtime registry. Registering into a fresh local registry
// reproduces that check without touching the global one.
func TestNewMetrics_RegistersCleanly(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(NewMetrics()); err != nil {
		t.Fatalf("registering nova metrics: %v", err)
	}
}

// TestMetrics_NilSafe asserts every recording helper is a no-op on a nil
// receiver, since the controllers and the decorator may be built without
// metrics (as the other tests in this package do).
func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics
	// None of these should panic.
	m.observeReconcile(outcomeSynced, time.Now())
	m.observePoll("success", time.Now())
	m.recordPollError("fetch_servers")
	m.setPollGauges(1, 2, 3)
	m.observeAPIRequest(apiMethodFetchServer, apiOutcomeSuccess, time.Now())
	m.recordPrune("pruned")

	// A nil-metrics decorator must still forward transparently.
	c := NewMeteredOpenStackClient(&fakeOpenStackClient{}, nil)
	if _, err := c.FetchServers(context.Background()); err != nil {
		t.Fatalf("FetchServers through nil-metrics decorator: %v", err)
	}
}

// TestMeteredOpenStackClient_Outcomes checks the decorator records the right
// method/outcome labels, including the found/notfound/error distinction on
// FetchServer that is easy to get wrong.
func TestMeteredOpenStackClient_Outcomes(t *testing.T) {
	const (
		knownUUID   = "11111111-1111-1111-1111-111111111111"
		missingUUID = "22222222-2222-2222-2222-222222222222"
		erroringID  = "33333333-3333-3333-3333-333333333333"
	)

	m := NewMetrics()
	reg := prometheus.NewRegistry()
	if err := reg.Register(m); err != nil {
		t.Fatalf("registering metrics: %v", err)
	}

	delegate := &fakeOpenStackClient{
		result: []servers.Server{{ID: knownUUID}},
		getErr: map[string]error{erroringID: errors.New("boom")},
	}
	c := NewMeteredOpenStackClient(delegate, m)
	ctx := context.Background()

	// success: server present
	if _, found, err := c.FetchServer(ctx, knownUUID); err != nil || !found {
		t.Fatalf("FetchServer(known) = found %v, err %v; want found, no error", found, err)
	}
	// notfound: Nova has no such server (nil error)
	if _, found, err := c.FetchServer(ctx, missingUUID); err != nil || found {
		t.Fatalf("FetchServer(missing) = found %v, err %v; want not-found, no error", found, err)
	}
	// error: delegate returns an error
	if _, _, err := c.FetchServer(ctx, erroringID); err == nil {
		t.Fatal("FetchServer(erroring) = nil error; want error")
	}
	// bulk list success
	if _, err := c.FetchServers(ctx); err != nil {
		t.Fatalf("FetchServers: %v", err)
	}

	want := `
# HELP vm_operator_nova_api_requests_total Total number of Nova API requests by method and outcome.
# TYPE vm_operator_nova_api_requests_total counter
vm_operator_nova_api_requests_total{method="fetch_server",outcome="error"} 1
vm_operator_nova_api_requests_total{method="fetch_server",outcome="notfound"} 1
vm_operator_nova_api_requests_total{method="fetch_server",outcome="success"} 1
vm_operator_nova_api_requests_total{method="fetch_servers",outcome="success"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "vm_operator_nova_api_requests_total"); err != nil {
		t.Error(err)
	}
}
