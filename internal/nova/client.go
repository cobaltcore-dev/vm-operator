// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

	// FetchFlavor returns the Nova flavor with the given id, including its extra
	// specs. The sync controller uses it on the create path to resolve a server's
	// vCPU, RAM, and hypervisor type. Extra specs are fetched explicitly (not read
	// off the Get response) because flavor Get/List only embed extra_specs at
	// microversion >=2.61.
	FetchFlavor(ctx context.Context, id string) (*flavors.Flavor, error)

	// FetchFlavors lists all flavors (public + private) with their extra specs,
	// keyed by flavor id. The poll loop uses it to filter out servers whose flavor
	// is not a hypervisor type this operator manages, before enqueuing.
	FetchFlavors(ctx context.Context) (map[string]*flavors.Flavor, error)
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

	// Region is the OpenStack region whose Nova endpoint to use (OS_REGION_NAME).
	// Required: the operator writes it to Spec.Region of VirtualMachines created
	// from Nova state and refuses to start when it is empty.
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
	httpClient, err := NewHTTPClient(ctx, cfg.HTTPClient)
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
//
// It walks Nova's pagination by hand rather than through
// gophercloud's servers.List(...).AllPages. AllPages accumulates every raw page
// body and only extracts at the end; on clouds with many thousands of servers
// that retains the full response set in memory at once, which we observed
// leaking in another service. Here each page is decoded straight into the
// result slice and its body closed before the next request, so only the
// servers themselves (not their intermediate page bodies) are held.
//
// The technical user watches servers across all projects, so we request with
// all_tenants. Nova excludes deleted servers by default, which is exactly the
// live snapshot we want. Duplicate IDs across page boundaries (possible when
// the server set shifts mid-walk) are dropped so callers never see the same
// uuid twice.
func (c *openStackClient) FetchServers(ctx context.Context) ([]servers.Server, error) {
	log := logf.FromContext(ctx)

	// ResourceBaseURL ends with a "/", so this yields e.g.
	// ".../compute/v2.1/servers/detail?all_tenants=true".
	initialURL := c.compute.ResourceBaseURL() + "servers/detail?all_tenants=true"
	nextURL := &initialURL

	var allServers []servers.Server
	seen := make(map[string]struct{})

	for nextURL != nil {
		page, next, err := c.fetchServerPage(ctx, *nextURL)
		if err != nil {
			return nil, err
		}
		for _, s := range page {
			if _, ok := seen[s.ID]; ok {
				log.V(1).Info("skipping duplicate nova server", "id", s.ID)
				continue
			}
			seen[s.ID] = struct{}{}
			allServers = append(allServers, s)
		}
		nextURL = next
	}

	return allServers, nil
}

// fetchServerPage fetches and decodes a single page of the detailed server
// listing at url, returning the servers on that page and the URL of the next
// page (nil when url is the last page). The response body is read and closed
// before returning so no per-page state outlives the call.
func (c *openStackClient) fetchServerPage(ctx context.Context, url string) ([]servers.Server, *string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, nil, fmt.Errorf("building nova servers request: %w", err)
	}
	req.Header.Set("X-Auth-Token", c.compute.Token())
	if c.compute.Microversion != "" {
		req.Header.Set("X-OpenStack-Nova-API-Version", c.compute.Microversion)
	}

	resp, err := c.compute.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("listing nova servers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("listing nova servers: unexpected status code %d", resp.StatusCode)
	}

	// servers.Server has a custom UnmarshalJSON, so decoding into it here is
	// equivalent to servers.ExtractServers over an AllPages result.
	var list struct {
		Servers []servers.Server `json:"servers"`
		Links   []struct {
			Rel  string `json:"rel"`
			Href string `json:"href"`
		} `json:"servers_links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, nil, fmt.Errorf("decoding nova servers: %w", err)
	}

	var next *string
	for _, link := range list.Links {
		if link.Rel == "next" {
			href := link.Href
			next = &href
			break
		}
	}
	return list.Servers, next, nil
}

// FetchServer returns the current state of a single Nova server by uuid.
//
// It does NOT use the member endpoint GET /servers/{id}: that call is scoped to
// the token's project, and its only cross-project lever, all_tenants, is not a
// valid parameter there so Nova silently ignores it — a server owned by another
// project then 404s and is misread as "gone". Instead it queries the list
// endpoint the same way FetchServers does (all_tenants=true, an admin path the
// technical user is allowed) with the admin-only uuid filter, which returns the
// one matching server across all projects. An empty result means the server is
// genuinely gone, reported as (nil, false, nil).
func (c *openStackClient) FetchServer(ctx context.Context, uuid string) (*servers.Server, bool, error) {
	// ResourceBaseURL ends with a "/", so this yields e.g.
	// ".../compute/v2.1/servers/detail?all_tenants=true&uuid={uuid}". Reusing the
	// detailed list endpoint keeps the server shape identical to FetchServers.
	url := c.compute.ResourceBaseURL() + "servers/detail?all_tenants=true&uuid=" + neturl.QueryEscape(uuid)

	page, _, err := c.fetchServerPage(ctx, url)
	if err != nil {
		return nil, false, fmt.Errorf("getting nova server %s: %w", uuid, err)
	}
	for i := range page {
		if page[i].ID == uuid {
			return &page[i], true, nil
		}
	}
	return nil, false, nil
}

// FetchFlavor returns the Nova flavor with the given id, including its extra
// specs (fetched separately so correctness does not depend on microversion >=2.61
// embedding extra_specs in the Get response).
func (c *openStackClient) FetchFlavor(ctx context.Context, id string) (*flavors.Flavor, error) {
	flavor, err := flavors.Get(ctx, c.compute, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("getting nova flavor %s: %w", id, err)
	}
	specs, err := flavors.ListExtraSpecs(ctx, c.compute, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("getting extra specs for nova flavor %s: %w", id, err)
	}
	flavor.ExtraSpecs = specs
	return flavor, nil
}

// FetchFlavors lists all flavors (public + private, across all projects — the
// technical user is an admin) and resolves each flavor's extra specs, returning
// them keyed by id. Extra specs are fetched per flavor via ListExtraSpecs rather
// than read off the list response, which only embeds them at microversion >=2.61.
//
// Flavor sets are small and stable, so listing via AllPages here is fine — unlike
// the server listing, there is no large-response memory concern to hand-paginate
// around.
func (c *openStackClient) FetchFlavors(ctx context.Context) (map[string]*flavors.Flavor, error) {
	pages, err := flavors.ListDetail(c.compute, flavors.ListOpts{AccessType: flavors.AllAccess}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing nova flavors: %w", err)
	}
	list, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return nil, fmt.Errorf("extracting nova flavors: %w", err)
	}

	out := make(map[string]*flavors.Flavor, len(list))
	for i := range list {
		flavor := list[i]
		specs, err := flavors.ListExtraSpecs(ctx, c.compute, flavor.ID).Extract()
		if err != nil {
			return nil, fmt.Errorf("getting extra specs for nova flavor %s: %w", flavor.ID, err)
		}
		flavor.ExtraSpecs = specs
		out[flavor.ID] = &flavor
	}
	return out, nil
}
