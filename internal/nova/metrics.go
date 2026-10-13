// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/prometheus/client_golang/prometheus"
)

// metricPrefix is the common prefix for every metric this package emits. It
// matches the prefix the rest of vm-operator uses (see the cortex
// multicluster monitor wired up in cmd/main.go) so all project metrics share a
// namespace.
const metricPrefix = "vm_operator_nova_"

// Reconcile outcome label values recorded on reconcile_total /
// reconcile_duration_seconds.
const (
	outcomeSynced  = "synced"  // existing resource synced to a live server
	outcomeCreated = "created" // resource created from a server Nova knows
	outcomeCleared = "cleared" // resource's Nova fields cleared (server gone)
	outcomeNoop    = "noop"    // neither side had the server; nothing to do
	outcomeError   = "error"   // reconcile returned an error
)

// Nova API request label values recorded on api_requests_total /
// api_request_duration_seconds.
const (
	apiMethodFetchServers = "fetch_servers"
	apiMethodFetchServer  = "fetch_server"
	apiMethodFetchFlavor  = "fetch_flavor"
	apiMethodFetchFlavors = "fetch_flavors"

	apiOutcomeSuccess  = "success"
	apiOutcomeNotFound = "notfound" // FetchServer: Nova has no such server
	apiOutcomeError    = "error"
)

// Metrics bundles the Prometheus instruments the nova controllers and the
// metered OpenStack client record into. It is created once with NewMetrics,
// injected into the controllers (and the metered OpenStack client), and
// registered once with the controller-runtime metrics Registry from main.
//
// Metrics implements prometheus.Collector so the whole set registers with a
// single MustRegister call, mirroring the cortex multiclusterMonitor already
// wired up in cmd/main.go.
//
// Every recording method is nil-safe: a nil *Metrics records nothing, so the
// controllers (and the decorator) can be constructed without metrics, as the
// unit tests do.
type Metrics struct {
	reconcileTotal     *prometheus.CounterVec
	reconcileDuration  *prometheus.HistogramVec
	pollDuration       *prometheus.HistogramVec
	pollErrorsTotal    *prometheus.CounterVec
	servers            prometheus.Gauge
	managedVMs         prometheus.Gauge
	reconcileBacklog   prometheus.Gauge
	apiRequestsTotal   *prometheus.CounterVec
	apiRequestDuration *prometheus.HistogramVec
	pruneTotal         *prometheus.CounterVec
}

// Compile-time assertion that *Metrics is a Collector.
var _ prometheus.Collector = (*Metrics)(nil)

// NewMetrics constructs the full set of nova instruments. The returned value is
// registered as a single Collector (see cmd/main.go); it does not self-register.
func NewMetrics() *Metrics {
	return &Metrics{
		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "reconcile_total",
			Help: "Total number of nova VirtualMachine reconciles by outcome.",
		}, []string{"outcome"}),
		reconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricPrefix + "reconcile_duration_seconds",
			Help:    "Duration of nova VirtualMachine reconciles in seconds, by outcome.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"outcome"}),
		pollDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricPrefix + "poll_duration_seconds",
			Help:    "Duration of a nova poll loop iteration in seconds, by result.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"result"}),
		pollErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "poll_errors_total",
			Help: "Total number of nova poll loop failures by stage.",
		}, []string{"stage"}),
		servers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricPrefix + "servers",
			Help: "Number of nova servers returned by the most recent successful poll.",
		}),
		managedVMs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricPrefix + "managed_vms",
			Help: "Number of VirtualMachine resources observed by the most recent successful poll.",
		}),
		reconcileBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: metricPrefix + "reconcile_backlog",
			Help: "Number of uuids the most recent poll enqueued for reconciliation.",
		}),
		apiRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "api_requests_total",
			Help: "Total number of Nova API requests by method and outcome.",
		}, []string{"method", "outcome"}),
		apiRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    metricPrefix + "api_request_duration_seconds",
			Help:    "Duration of Nova API requests in seconds, by method.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method"}),
		pruneTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metricPrefix + "prune_total",
			Help: "Total number of VirtualMachine prunes by the cleanup controller, by outcome.",
		}, []string{"outcome"}),
	}
}

// collectors returns every instrument so Describe/Collect can forward to them.
func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.reconcileTotal, m.reconcileDuration,
		m.pollDuration, m.pollErrorsTotal,
		m.servers, m.managedVMs, m.reconcileBacklog,
		m.apiRequestsTotal, m.apiRequestDuration,
		m.pruneTotal,
	}
}

// Describe implements prometheus.Collector.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range m.collectors() {
		c.Describe(ch)
	}
}

// Collect implements prometheus.Collector.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	for _, c := range m.collectors() {
		c.Collect(ch)
	}
}

// observeReconcile records the outcome and duration of a single reconcile.
func (m *Metrics) observeReconcile(outcome string, start time.Time) {
	if m == nil {
		return
	}
	m.reconcileTotal.WithLabelValues(outcome).Inc()
	m.reconcileDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// observePoll records the duration of a poll iteration under the given result
// ("success" or "error").
func (m *Metrics) observePoll(result string, start time.Time) {
	if m == nil {
		return
	}
	m.pollDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// recordPollError counts a poll failure at the given stage.
func (m *Metrics) recordPollError(stage string) {
	if m == nil {
		return
	}
	m.pollErrorsTotal.WithLabelValues(stage).Inc()
}

// setPollGauges records the fleet-size snapshot from a successful poll.
func (m *Metrics) setPollGauges(servers, managedVMs, backlog int) {
	if m == nil {
		return
	}
	m.servers.Set(float64(servers))
	m.managedVMs.Set(float64(managedVMs))
	m.reconcileBacklog.Set(float64(backlog))
}

// observeAPIRequest records the outcome and duration of a single Nova API call.
func (m *Metrics) observeAPIRequest(method, outcome string, start time.Time) {
	if m == nil {
		return
	}
	m.apiRequestsTotal.WithLabelValues(method, outcome).Inc()
	m.apiRequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

// recordPrune counts a prune attempt by the cleanup controller.
func (m *Metrics) recordPrune(outcome string) {
	if m == nil {
		return
	}
	m.pruneTotal.WithLabelValues(outcome).Inc()
}

// meteredOpenStackClient wraps an OpenStackClient and records request count,
// latency, and outcome around each Nova call. The gophercloud layer in
// client.go stays metrics-free; main wraps the real client with this so all
// Nova traffic is instrumented in one place.
type meteredOpenStackClient struct {
	delegate OpenStackClient
	metrics  *Metrics
}

// Compile-time assertion that the decorator satisfies the interface.
var _ OpenStackClient = (*meteredOpenStackClient)(nil)

// NewMeteredOpenStackClient wraps delegate so its calls are instrumented into
// m. A nil m yields a transparent (no-op metrics) wrapper.
func NewMeteredOpenStackClient(delegate OpenStackClient, m *Metrics) OpenStackClient {
	return &meteredOpenStackClient{delegate: delegate, metrics: m}
}

// FetchServers records the bulk-list call around the delegate.
func (c *meteredOpenStackClient) FetchServers(ctx context.Context) ([]servers.Server, error) {
	start := time.Now()
	out, err := c.delegate.FetchServers(ctx)
	outcome := apiOutcomeSuccess
	if err != nil {
		outcome = apiOutcomeError
	}
	c.metrics.observeAPIRequest(apiMethodFetchServers, outcome, start)
	return out, err
}

// FetchServer records the per-server get around the delegate, distinguishing a
// not-found (server gone) from a genuine error.
func (c *meteredOpenStackClient) FetchServer(ctx context.Context, uuid string) (*servers.Server, bool, error) {
	start := time.Now()
	server, found, err := c.delegate.FetchServer(ctx, uuid)
	outcome := apiOutcomeSuccess
	switch {
	case err != nil:
		outcome = apiOutcomeError
	case !found:
		outcome = apiOutcomeNotFound
	}
	c.metrics.observeAPIRequest(apiMethodFetchServer, outcome, start)
	return server, found, err
}

// FetchFlavor records the flavor get around the delegate.
func (c *meteredOpenStackClient) FetchFlavor(ctx context.Context, id string) (*flavors.Flavor, error) {
	start := time.Now()
	flavor, err := c.delegate.FetchFlavor(ctx, id)
	outcome := apiOutcomeSuccess
	if err != nil {
		outcome = apiOutcomeError
	}
	c.metrics.observeAPIRequest(apiMethodFetchFlavor, outcome, start)
	return flavor, err
}

// FetchFlavors records the flavor list around the delegate.
func (c *meteredOpenStackClient) FetchFlavors(ctx context.Context) (map[string]*flavors.Flavor, error) {
	start := time.Now()
	out, err := c.delegate.FetchFlavors(ctx)
	outcome := apiOutcomeSuccess
	if err != nil {
		outcome = apiOutcomeError
	}
	c.metrics.observeAPIRequest(apiMethodFetchFlavors, outcome, start)
	return out, err
}
