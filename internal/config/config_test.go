// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

type testConfig struct {
	Name    string `json:"name"`
	Replica int    `json:"replica"`
	Enabled bool   `json:"enabled"`
}

// writeFile writes content to a temp file and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	path := writeFile(t, `{"name":"vm-operator","replica":3,"enabled":true}`)

	cfg, err := Load[testConfig](path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := testConfig{Name: "vm-operator", Replica: 3, Enabled: true}
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	cfg, err := Load[testConfig](missing)
	if err == nil {
		t.Fatalf("expected error for missing file, got nil (result %+v)", cfg)
	}
	if cfg != (testConfig{}) {
		t.Errorf("expected zero config on error, got %+v", cfg)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	path := writeFile(t, `{"name":"vm-operator",`)

	cfg, err := Load[testConfig](path)
	if err == nil {
		t.Fatalf("expected error for malformed JSON, got nil (result %+v)", cfg)
	}
	if cfg != (testConfig{}) {
		t.Errorf("expected zero config on error, got %+v", cfg)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	path := writeFile(t, ``)

	// An empty file is not valid JSON and must surface a parse error.
	if _, err := Load[testConfig](path); err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestLoad_PartialJSON(t *testing.T) {
	path := writeFile(t, `{"name":"vm-operator"}`)

	cfg, err := Load[testConfig](path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Omitted fields default to their zero values.
	want := testConfig{Name: "vm-operator"}
	if cfg != want {
		t.Errorf("Load = %+v, want %+v", cfg, want)
	}
}

func TestLoad_MapType(t *testing.T) {
	path := writeFile(t, `{"a":1,"b":2}`)

	cfg, err := Load[map[string]int](path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg["a"] != 1 || cfg["b"] != 2 {
		t.Errorf("Load = %v, want map[a:1 b:2]", cfg)
	}
}
