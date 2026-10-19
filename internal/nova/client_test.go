// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestOpenStackClientConfig_authOptions(t *testing.T) {
	cfg := OpenStackClientConfig{
		URL:                 "https://keystone.example.com/v3",
		OSUsername:          "tech-user",
		OSPassword:          "secret",
		OSUserDomainName:    "Default",
		OSProjectName:       "service",
		OSProjectDomainName: "ServiceDomain",
	}

	got := cfg.authOptions()

	if got.IdentityEndpoint != cfg.URL {
		t.Errorf("IdentityEndpoint = %q, want %q", got.IdentityEndpoint, cfg.URL)
	}
	if got.Username != cfg.OSUsername || got.Password != cfg.OSPassword {
		t.Errorf("credentials not mapped: %+v", got)
	}
	if got.DomainName != cfg.OSUserDomainName {
		t.Errorf("DomainName = %q, want %q", got.DomainName, cfg.OSUserDomainName)
	}
	if !got.AllowReauth {
		t.Error("AllowReauth should be true for a long-running client")
	}
	if got.Scope == nil {
		t.Fatal("expected a project scope")
	}
	if got.Scope.ProjectName != cfg.OSProjectName || got.Scope.DomainName != cfg.OSProjectDomainName {
		t.Errorf("scope = %+v, want project %q domain %q", got.Scope, cfg.OSProjectName, cfg.OSProjectDomainName)
	}
}

// newTestOpenStackClient returns an openStackClient whose ServiceClient points at srv,
// carrying the given auth token and microversion. srv.URL is suffixed with "/"
// to mirror the trailing slash gophercloud guarantees on real endpoints.
func newTestOpenStackClient(srv *httptest.Server, token, microversion string) *openStackClient {
	provider := &gophercloud.ProviderClient{TokenID: token}
	compute := &gophercloud.ServiceClient{
		ProviderClient: provider,
		Endpoint:       srv.URL + "/",
		Microversion:   microversion,
	}
	return &openStackClient{compute: compute}
}

// writeJSON writes body as the response, failing the test on a write error so
// errcheck (check-blank) stays satisfied without silently dropping the error.
func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

func TestFetchServers_singlePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The listing must be scoped to every project.
		if got := r.URL.Query().Get("all_tenants"); got != "true" {
			t.Errorf("all_tenants = %q, want \"true\"", got)
		}
		writeJSON(t, w, `{"servers":[{"id":"a","status":"ACTIVE"},{"id":"b","status":"SHUTOFF"}]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	got, err := client.FetchServers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d servers, want 2", len(got))
	}
	if got[0].ID != "a" || got[0].Status != "ACTIVE" {
		t.Errorf("server[0] = %+v, want id=a status=ACTIVE", got[0])
	}
	if got[1].ID != "b" || got[1].Status != "SHUTOFF" {
		t.Errorf("server[1] = %+v, want id=b status=SHUTOFF", got[1])
	}
}

func TestFetchServers_followsPagination(t *testing.T) {
	var nextURL string // set once the server is up, so we can hand out an absolute "next" link.
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		switch r.URL.Query().Get("marker") {
		case "":
			// First page points at itself with a marker for the second page.
			writeJSON(t, w, `{"servers":[{"id":"a"}],"servers_links":[{"rel":"next","href":"`+nextURL+`?marker=a"}]}`)
		case "a":
			// Second (and last) page: no next link.
			writeJSON(t, w, `{"servers":[{"id":"b"}]}`)
		default:
			t.Errorf("unexpected marker %q", r.URL.Query().Get("marker"))
		}
	}))
	defer srv.Close()
	nextURL = srv.URL + "/servers/detail"

	client := newTestOpenStackClient(srv, "tok", "")
	got, err := client.FetchServers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pages != 2 {
		t.Errorf("fetched %d pages, want 2", pages)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("got %+v, want servers a then b", got)
	}
}

func TestFetchServers_dropsDuplicates(t *testing.T) {
	var nextURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("marker") {
		case "":
			// "a" appears on both pages (e.g. the set shifted mid-walk).
			writeJSON(t, w, `{"servers":[{"id":"a"},{"id":"b"}],"servers_links":[{"rel":"next","href":"`+nextURL+`?marker=b"}]}`)
		default:
			writeJSON(t, w, `{"servers":[{"id":"a"},{"id":"c"}]}`)
		}
	}))
	defer srv.Close()
	nextURL = srv.URL + "/servers/detail"

	client := newTestOpenStackClient(srv, "tok", "")
	got, err := client.FetchServers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.ID
	}
	// "a" is kept once, in the order first seen.
	if strings.Join(ids, ",") != "a,b,c" {
		t.Errorf("ids = %v, want [a b c]", ids)
	}
}

func TestFetchServers_sendsAuthAndMicroversionHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Auth-Token"); got != "secret-token" {
			t.Errorf("X-Auth-Token = %q, want \"secret-token\"", got)
		}
		if got := r.Header.Get("X-OpenStack-Nova-API-Version"); got != "2.66" {
			t.Errorf("microversion header = %q, want \"2.66\"", got)
		}
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "secret-token", "2.66")
	if _, err := client.FetchServers(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestFetchServers_reauthenticatesOn401 pins the fix for the silent-token-expiry
// bug: because the server-page requests are dispatched with the plain HTTPClient
// (bypassing gophercloud's ServiceClient.Request, where its 401 reauth lives),
// FetchServers must do the reauth-and-retry itself. Here the fake Nova rejects
// the stale token with a 401, the ReauthFunc installed on the provider mints a
// fresh one, and the retry must carry that fresh token and succeed.
func TestFetchServers_reauthenticatesOn401(t *testing.T) {
	var sawTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("X-Auth-Token")
		sawTokens = append(sawTokens, tok)
		if tok != "fresh-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(t, w, `{"servers":[{"id":"a","status":"ACTIVE"}]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "stale-token", "")
	// Mirror the production wiring that openstack.Authenticate installs for an
	// AllowReauth client: a token lock plus a ReauthFunc that mints a new token.
	var reauths int
	client.compute.UseTokenLock()
	client.compute.ReauthFunc = func(context.Context) error {
		reauths++
		client.compute.SetToken("fresh-token")
		return nil
	}

	got, err := client.FetchServers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reauths != 1 {
		t.Errorf("ReauthFunc called %d times, want exactly 1", reauths)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("servers = %+v, want the single server returned after reauth", got)
	}
	want := []string{"stale-token", "fresh-token"}
	if len(sawTokens) != len(want) || sawTokens[0] != want[0] || sawTokens[1] != want[1] {
		t.Errorf("tokens seen by server = %v, want %v (stale first, fresh on retry)", sawTokens, want)
	}
}

// TestFetchServers_non401ErrorDoesNotReauth makes sure the reauth path is scoped
// to 401 only: a 500 must surface as an error without burning a reauth.
func TestFetchServers_non401ErrorDoesNotReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	var reauths int
	client.compute.UseTokenLock()
	client.compute.ReauthFunc = func(context.Context) error {
		reauths++
		return nil
	}

	if _, err := client.FetchServers(context.Background()); err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if reauths != 0 {
		t.Errorf("ReauthFunc called %d times for a 500, want 0", reauths)
	}
}

func TestFetchServers_omitsMicroversionHeaderWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Openstack-Nova-Api-Version"]; ok {
			t.Error("microversion header should be absent when no microversion is configured")
		}
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	if _, err := client.FetchServers(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFetchServers_errorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	_, err := client.FetchServers(context.Background())
	if err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status code", err)
	}
}

func TestFetchServers_errorOnMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"servers": not json`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	if _, err := client.FetchServers(context.Background()); err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
}

func TestFetchServers_contextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestOpenStackClient(srv, "tok", "")
	if _, err := client.FetchServers(ctx); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
}

func TestFetchServer_requestsAllTenantsAndUUIDFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// FetchServer must query the list endpoint (not the project-scoped member
		// GET) with the admin all_tenants + uuid filter, so a server owned by another
		// project is still returned.
		if got := r.URL.Path; got != "/servers/detail" {
			t.Errorf("path = %q, want /servers/detail", got)
		}
		if got := r.URL.Query().Get("all_tenants"); got != "true" {
			t.Errorf("all_tenants = %q, want \"true\"", got)
		}
		if got := r.URL.Query().Get("uuid"); got != "a" {
			t.Errorf("uuid = %q, want \"a\"", got)
		}
		if got := r.Header.Get("X-Auth-Token"); got != "tok" {
			t.Errorf("X-Auth-Token = %q, want \"tok\"", got)
		}
		if got := r.Header.Get("X-OpenStack-Nova-API-Version"); got != "2.66" {
			t.Errorf("microversion header = %q, want \"2.66\"", got)
		}
		writeJSON(t, w, `{"servers":[{"id":"a","status":"ACTIVE"}]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "2.66")
	server, found, err := client.FetchServer(context.Background(), "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if server.ID != "a" || server.Status != "ACTIVE" {
		t.Errorf("server = %+v, want id=a status=ACTIVE", server)
	}
}

func TestFetchServer_emptyResultIsGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No matching server across any project: an empty list, not a 404.
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	server, found, err := client.FetchServer(context.Background(), "missing")
	if err != nil {
		t.Fatalf("an empty result must be reported as gone, not an error: %v", err)
	}
	if found || server != nil {
		t.Errorf("got (found=%v, server=%+v), want (false, nil)", found, server)
	}
}

func TestFetchServer_errorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	if _, _, err := client.FetchServer(context.Background(), "a"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

// A server absent from the live listing but present as soft-deleted is surfaced
// with status=DELETED, via a fallback query carrying the admin deleted toggle,
// so the controller can route it to the deferred-deletion path.
func TestFetchServer_fallsBackToDeletedServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deleted") == "true" {
			// Deleted lookup must still be the all_tenants + uuid admin query.
			if got := r.URL.Query().Get("all_tenants"); got != "true" {
				t.Errorf("deleted query all_tenants = %q, want \"true\"", got)
			}
			if got := r.URL.Query().Get("uuid"); got != "a" {
				t.Errorf("deleted query uuid = %q, want \"a\"", got)
			}
			writeJSON(t, w, `{"servers":[{"id":"a","status":"DELETED"}]}`)
			return
		}
		// Live lookup: not found.
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	server, found, err := client.FetchServer(context.Background(), "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true for a soft-deleted server")
	}
	if server.ID != "a" || server.Status != "DELETED" {
		t.Errorf("server = %+v, want id=a status=DELETED", server)
	}
}

// When the server is live, the authoritative live lookup wins and the deleted
// fallback is never issued.
func TestFetchServer_prefersLiveOverDeleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deleted") == "true" {
			t.Error("deleted fallback must not run when the server is live")
			writeJSON(t, w, `{"servers":[{"id":"a","status":"DELETED"}]}`)
			return
		}
		writeJSON(t, w, `{"servers":[{"id":"a","status":"ACTIVE"}]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	server, found, err := client.FetchServer(context.Background(), "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found || server.Status != "ACTIVE" {
		t.Errorf("got (found=%v, server=%+v), want (true, ACTIVE)", found, server)
	}
}

// Absent from both the live and deleted listings: genuinely gone.
func TestFetchServer_goneWhenAbsentFromBoth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	server, found, err := client.FetchServer(context.Background(), "missing")
	if err != nil {
		t.Fatalf("absence from both must be gone, not an error: %v", err)
	}
	if found || server != nil {
		t.Errorf("got (found=%v, server=%+v), want (false, nil)", found, server)
	}
}

// A failure on the deleted fallback query is returned, not misreported as gone.
func TestFetchServer_errorOnDeletedQueryNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("deleted") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(t, w, `{"servers":[]}`)
	}))
	defer srv.Close()

	client := newTestOpenStackClient(srv, "tok", "")
	if _, _, err := client.FetchServer(context.Background(), "a"); err == nil {
		t.Fatal("expected an error when the deleted fallback query fails")
	}
}
