// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
)

// OpenStackClient abstracts the OpenStack Nova API as far as this controller
// needs it: it lists the current Nova servers so the controller can reconcile
// them against the VirtualMachine CRDs.
//
// Servers are returned as gophercloud's own servers.Server; the controller
// reads the fields it needs directly (ID, Status, OS-EXT-* extended
// attributes) rather than going through a translation layer.
type OpenStackClient interface {
	// FetchServers returns all non-deleted Nova servers as they exist right
	// now. The poll loop diffs this snapshot against the VM CRDs to decide
	// which uuids to enqueue for reconciliation.
	FetchServers(ctx context.Context) ([]servers.Server, error)

	// FetchServer returns the current state of a single Nova server by uuid.
	// found is false (with a nil error) when Nova has no such server, which the
	// reconciler treats as "the server is gone". Reconcile calls this to get
	// authoritative per-server state rather than relying on the bulk snapshot.
	FetchServer(ctx context.Context, uuid string) (server *servers.Server, found bool, err error)
}

// OpenStackClientConfig supplies everything NewOpenStackClient needs to reach
// Nova: the technical user's Keystone credentials, the service-catalog
// endpoint to use, the Nova microversion, and an optional custom HTTP client
// (e.g. to trust a specific CA bundle). Field tags mirror the OpenStack CLI
// environment variables so the config can be loaded straight from JSON.
type OpenStackClientConfig struct {
	// URL is the Keystone identity endpoint (OS_AUTH_URL).
	URL string `json:"url"`
	// Availability selects which service-catalog endpoint to talk to:
	// "public", "internal", or "admin". Defaults to "public" when empty.
	Availability gophercloud.Availability `json:"availability"`
	// OSUsername is the OpenStack username (OS_USERNAME).
	OSUsername string `json:"username"`
	// OSPassword is the OpenStack password (OS_PASSWORD).
	OSPassword string `json:"password"`
	// OSProjectName is the OpenStack project name (OS_PROJECT_NAME).
	OSProjectName string `json:"projectName"`
	// OSUserDomainName is the OpenStack user domain name (OS_USER_DOMAIN_NAME).
	OSUserDomainName string `json:"userDomainName"`
	// OSProjectDomainName is the OpenStack project domain name
	// (OS_PROJECT_DOMAIN_NAME).
	OSProjectDomainName string `json:"projectDomainName"`

	// Region is the OpenStack region whose Nova endpoint to use. May be empty
	// for single-region clouds.
	Region string `json:"region,omitempty"`

	// Microversion is the Nova API microversion to request, e.g. "2.66". Empty
	// means the provider default. A recent microversion is needed for the
	// extended server attributes (task_state, vm_state, host) the controller
	// relies on.
	Microversion string `json:"microversion,omitempty"`

	// HTTPClient configures the underlying HTTP client, e.g. a client TLS
	// certificate or trusting a self-signed server certificate. The zero value
	// yields a plain client.
	HTTPClient HTTPClientConfig `json:"httpClient,omitempty"`
}

// openStackClient is the default OpenStackClient, backed by gophercloud.
//
// Each FetchServers call lists all current servers across every project. There
// is deliberately no incremental changes-since bookkeeping or local cache: the
// controller computes what changed by diffing this snapshot against the VM
// CRDs, which also lets it detect CRDs whose Nova server no longer exists
// (they simply will not appear in the snapshot).
type openStackClient struct {
	compute *gophercloud.ServiceClient
}

// Compile-time assertion that openStackClient satisfies the interface.
var _ OpenStackClient = (*openStackClient)(nil)

// authOptions maps the config onto gophercloud's auth options for the technical
// user, scoped to the configured project and domain.
func (cfg OpenStackClientConfig) authOptions() gophercloud.AuthOptions {
	return gophercloud.AuthOptions{
		IdentityEndpoint: cfg.URL,
		Username:         cfg.OSUsername,
		Password:         cfg.OSPassword,
		DomainName:       cfg.OSUserDomainName,
		// We hold a long-running client, so let gophercloud transparently
		// re-authenticate when the token expires.
		AllowReauth: true,
		Scope: &gophercloud.AuthScope{
			ProjectName: cfg.OSProjectName,
			DomainName:  cfg.OSProjectDomainName,
		},
	}
}

// NewOpenStackClient authenticates the technical user against Keystone and
// returns an OpenStackClient talking to the configured Nova endpoint.
func NewOpenStackClient(ctx context.Context, cfg OpenStackClientConfig) (OpenStackClient, error) {
	authOpts := cfg.authOptions()

	provider, err := openstack.NewClient(authOpts.IdentityEndpoint)
	if err != nil {
		return nil, fmt.Errorf("creating openstack provider client: %w", err)
	}

	// Install a custom HTTP client so we can trust a specific CA bundle,
	// present a client certificate, and tag requests with our User-Agent.
	httpClient, err := NewHTTPClient(cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("configuring openstack http client: %w", err)
	}
	provider.HTTPClient = *httpClient

	if err := openstack.Authenticate(ctx, provider, authOpts); err != nil {
		return nil, fmt.Errorf("authenticating with openstack: %w", err)
	}

	compute, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region:       cfg.Region,
		Availability: cfg.Availability,
	})
	if err != nil {
		return nil, fmt.Errorf("creating nova compute client: %w", err)
	}
	if cfg.Microversion != "" {
		compute.Microversion = cfg.Microversion
	}

	return &openStackClient{compute: compute}, nil
}

// FetchServers lists all non-deleted Nova servers across all projects.
func (c *openStackClient) FetchServers(ctx context.Context) ([]servers.Server, error) {
	// The technical user watches servers across all projects, so list with
	// all_tenants. Nova excludes deleted servers by default, which is exactly
	// the live snapshot we want.
	opts := servers.ListOpts{AllTenants: true}

	page, err := servers.List(c.compute, opts).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing nova servers: %w", err)
	}
	out, err := servers.ExtractServers(page)
	if err != nil {
		return nil, fmt.Errorf("extracting nova servers: %w", err)
	}
	return out, nil
}

// FetchServer returns the current state of a single Nova server. A 404 from
// Nova is reported as (nil, false, nil) so the reconciler can treat it as "the
// server is gone" rather than an error.
func (c *openStackClient) FetchServer(ctx context.Context, uuid string) (*servers.Server, bool, error) {
	server, err := servers.Get(ctx, c.compute, uuid).Extract()
	if err != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("getting nova server %s: %w", uuid, err)
	}
	return server, true, nil
}
