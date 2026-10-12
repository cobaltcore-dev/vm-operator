// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Load reads the arbitrary configuration from a JSON file.
//
// The config file is expected mounted into the manager pod from a
// Kubernetes Secret templated by the Helm chart (see dist/templates/secret.yaml).
func Load[C any](path string) (C, error) {
	var cfg C
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %q: %w", path, err)
	}
	return cfg, nil
}
