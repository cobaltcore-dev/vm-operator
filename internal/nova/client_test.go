// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"testing"
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
