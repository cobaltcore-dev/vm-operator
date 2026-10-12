// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/cobaltcore-dev/cortex/pkg/multicluster"
)

// LoadClientConfig reads the multicluster client configuration from a JSON file.
//
// The file is mounted into the manager pod from a Kubernetes Secret templated by
// the Helm chart (see dist/templates/secret.yaml). It contains the full
// multicluster.ClientConfig, including any remote apiserver CA certificates.
func LoadClientConfig(path string) (multicluster.ClientConfig, error) {
	var cfg multicluster.ClientConfig

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading multicluster config %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing multicluster config %q: %w", path, err)
	}
	return cfg, nil
}
