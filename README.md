<!--
# SPDX-FileCopyrightText: Copyright 2024 SAP SE or an SAP affiliate company and cobaltcore-dev contributors
#
# SPDX-License-Identifier: Apache-2.0
-->

# VM Operator

## About this project

The VM Operator is a project designed to manage virtual machines efficiently within a cloud-native environment. It provides automation for the lifecycle management and scheduling of VMs, ensuring seamless integration with the CobaltCore stack.

## Requirements and Setup

The VM Operator ships a `VirtualMachine` CRD and a controller-manager. Install
everything (CRD, manager, RBAC, and the metrics Service/ServiceMonitor) with
Helm:

```bash
helm install vm-operator ./dist --set owner-info.support-group=<your-group>
```

The chart is gated by a few top-level values so you can install only what you
need: `manager.enabled`, `rbac.enabled`, `monitoring.enabled`, and `crd.enable`.
Note that `monitoring.enabled=true` requires the Prometheus Operator's
`monitoring.coreos.com/v1` CRDs to be present in the cluster.

### Multicluster client

The manager reconciles `VirtualMachine` resources through cortex's
[multicluster client](https://github.com/cobaltcore-dev/cortex/tree/main/pkg/multicluster),
so a single manager can serve VMs from its home cluster and, optionally, from
remote apiservers. Writes are routed to the cluster whose
`labels.availabilityZone` matches the VM's `spec.az`.

The client is configured entirely from Helm values under `multicluster.config`.
That whole block — including any remote `caCert` — is rendered into a Kubernetes
Secret and mounted into the manager pod at `/etc/vm-operator/multicluster.json`;
the manager loads it at startup. By default only the home cluster is used:

```yaml
multicluster:
  enabled: true
  config:
    apiservers:
      home:
        gvks:
          - cobaltcore.cloud/v1alpha1/VirtualMachine
      remotes: []   # see values.yaml for the remote apiserver schema
```

Each remote apiserver must accept the manager ServiceAccount's tokens
(Kubernetes structured auth); no extra RBAC is needed for home-cluster VMs.
Set `multicluster.enabled=false` to drop the Secret, mount, and flag entirely.

## Local development with Tilt

[Tilt](https://tilt.dev) gives you a fast build/deploy/reconcile loop against a
local cluster. The [`Tiltfile`](Tiltfile) builds the operator image from the
[`Dockerfile`](Dockerfile), deploys the Helm chart with that freshly built
image wired in, applies [`samples/vm.yaml`](samples/vm.yaml) so the operator has
a `VirtualMachine` to reconcile, and brings up a local monitoring stack so the
chart's metrics, `ServiceMonitor`, and alerts are actually scraped and
evaluated.

The monitoring stack consists of:

- The **Prometheus operator** and its `monitoring.coreos.com` CRDs, plus a
  **Prometheus** and **Alertmanager** instance, from the vendored dev chart in
  [`dev/helm/prometheus-operator`](dev/helm/prometheus-operator) (which depends
  on `kube-prometheus-stack` with all its extra components disabled). Because
  this installs the CRDs, the vm-operator chart's `monitoring` and `alerts`
  templates are enabled locally.
- **Perses** for dashboards, deployed from its upstream Helm repo with the
  dashboards under [`dashboards/`](dashboards) provisioned via a labelled
  ConfigMap.

Prerequisites:

- [Tilt](https://docs.tilt.dev/install.html), [Helm](https://helm.sh), and a
  local Docker daemon.
- A **throwaway/dev** Kubernetes cluster (e.g. [kind](https://kind.sigs.k8s.io)
  or Docker Desktop). Tilt deploys to whatever your **current `kubectl`
  context** points at, so double-check it first:

  ```bash
  kubectl config current-context
  ```

Then start the loop from the repo root:

```bash
tilt up
```

Tilt opens a UI (default <http://localhost:10350>) where you can watch the image
build, the chart deploy, the sample `VirtualMachine` being applied, and the
monitoring stack come up. Edit the Go sources and Tilt rebuilds and redeploys
automatically. Handy port-forwards exposed by the Tiltfile:

- Prometheus: <http://localhost:9090> (and its alerts at
  <http://localhost:9090/alerts>)
- Perses: <http://localhost:8080>

Tear everything down with:

```bash
tilt down
```

## Support, Feedback, Contributing

This project is open to feature requests/suggestions, bug reports etc. via [GitHub issues](https://github.com/cobaltcore-dev/vm-operator/issues). Contribution and feedback are encouraged and always welcome. For more information about how to contribute, the project structure, as well as additional contribution information, see our [Contribution Guidelines](CONTRIBUTING.md).

## Security / Disclosure
If you find any bug that may be a security problem, please follow our instructions at [in our security policy](https://github.com/cobaltcore-dev/vm-operator/security/policy) on how to report it. Please do not create GitHub issues for security-related doubts or problems.

## Code of Conduct

We as members, contributors, and leaders pledge to make participation in our community a harassment-free experience for everyone. By participating in this project, you agree to abide by its [Code of Conduct](https://github.com/cobaltcore-dev/.github/blob/main/CODE_OF_CONDUCT.md) at all times.

## Licensing

Copyright (20xx-)20xx SAP SE or an SAP affiliate company and cobaltcore-dev contributors. Please see our [LICENSE](LICENSE) for copyright and license information. Detailed information including third-party components and their licensing/copyright information is available [via the REUSE tool](https://api.reuse.software/info/github.com/cobaltcore-dev/vm-operator).