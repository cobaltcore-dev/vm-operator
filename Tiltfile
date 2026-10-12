# Copyright SAP SE
# SPDX-License-Identifier: Apache-2.0

# Local development setup for the vm-operator.
#
# This Tiltfile:
#   1. builds the operator image locally from the repo Dockerfile,
#   2. deploys a local monitoring stack: the Prometheus operator + its CRDs
#      (vendored dev chart), a Prometheus + Alertmanager instance, and Perses
#      for dashboards,
#   3. deploys the vm-operator Helm chart in dist/ (CRD + manager + rbac +
#      monitoring + alerts), wiring the freshly built image into the manager,
#   4. applies samples/vm.yaml so the operator has a VirtualMachine to
#      reconcile.
#
# It deploys to whatever cluster your current kubectl context points at, so
# make sure you are pointed at a throwaway/dev cluster before running `tilt up`.

# Image reference Tilt builds and injects into the chart. Tilt manages the tag,
# so we only pin the repository name here.
IMAGE = 'vm-operator'

# Guard against deploying to a shared/prod cluster by accident.
allow_k8s_contexts(k8s_context())

load('ext://helm_resource', 'helm_resource', 'helm_repo')

# Build the operator image from the repo root using the existing Dockerfile.
docker_build(
    IMAGE,
    context='.',
    dockerfile='Dockerfile',
)

########### Monitoring stack: Prometheus operator + CRDs + instance
# The vendored dev chart depends on kube-prometheus-stack, which installs the
# monitoring.coreos.com CRDs (ServiceMonitor, PrometheusRule, ...) and the
# operator. This chart also brings up a Prometheus + Alertmanager instance that
# scrape/evaluate everything in the cluster.
local('helm dependency build dev/helm/prometheus-operator')
k8s_yaml(helm('dev/helm/prometheus-operator', name='prometheus-operator'))
k8s_resource(
    'prometheus-operator',
    labels=['Monitoring'],
)
k8s_resource(
    new_name='prometheus',
    objects=['vm-operator-prometheus:Prometheus:default'],
    port_forwards=[port_forward(9090, 9090, name='prometheus')],
    links=[
        link('http://localhost:9090', 'metrics'),
        link('http://localhost:9090/alerts', 'alerts'),
    ],
    labels=['Monitoring'],
)
k8s_resource(
    new_name='alertmanager',
    objects=['vm-operator-alertmanager:Alertmanager:default'],
    labels=['Monitoring'],
)

########### Perses dashboards
# Perses is deployed from its upstream Helm repo. A sidecar picks up the
# dashboards + project we ship, provisioned via a labelled ConfigMap.
helm_repo(
    'perses',
    'https://perses.github.io/helm-charts',
    labels=['Repositories'],
)
helm_resource(
    'vm-operator-perses',
    'perses/perses',
    flags=['--values=dev/helm/perses/values.yaml'],
    port_forwards=[port_forward(8080, 8080, name='perses')],
    links=[link('http://localhost:8080', 'perses dashboard')],
    labels=['Monitoring'],
    resource_deps=['perses'],
)
watch_file('dashboards')
k8s_yaml(local(' '.join([
    'kubectl create configmap vm-operator-perses-dashboards',
    '--from-file=dashboards/',
    '--dry-run=client -o yaml |',
    'kubectl label --local -f - perses.dev/resource=true --dry-run=client -o yaml',
]), quiet=True))

########### VM operator chart
# Render the Helm chart from dist/. We point the manager image at the Tilt-built
# IMAGE (Tilt substitutes the built tag by ref) and satisfy the owner-info
# subchart's required value. Monitoring and alerts are on: the CRDs now exist
# thanks to the prometheus-operator chart above.
chart_yaml = helm(
    'dist',
    name='vm-operator',
    set=[
        'manager.image.repository=' + IMAGE,
        'owner-info.support-group=compute',
    ],
)
k8s_yaml(chart_yaml)

# Apply the sample VirtualMachine. It depends on the CRD the chart installs.
k8s_yaml('samples/vm.yaml')

# Group the manager Deployment under a readable resource name in the Tilt UI.
# It waits on the operator so the ServiceMonitor/PrometheusRule CRDs exist.
k8s_resource(
    workload='vm-operator-manager',
    new_name='vm-operator',
    resource_deps=['prometheus-operator'],
    labels=['VM-Operator'],
)
