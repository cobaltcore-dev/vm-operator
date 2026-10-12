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

# `helm dependency build` (below) writes the fetched dependency chart and helm's
# transient scratch dirs into dev/helm/prometheus-operator/. Those live under a
# watched path, so without ignoring them Tilt sees the writes, reloads the
# Tiltfile, rebuilds the dependency, and loops forever. These are all build
# artifacts (charts/*.tgz is gitignored; tmpcharts-* are helm scratch dirs).
watch_settings(ignore=[
    'dev/helm/prometheus-operator/charts',
    'dev/helm/prometheus-operator/tmpcharts-*',
    'dev/helm/prometheus-operator/Chart.lock',
])

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
#
# OpenStack credentials for the Nova sync controller are taken from your shell's
# standard OS_* environment variables (source your openrc before `tilt up`), so
# no secrets are committed. If OS_AUTH_URL is unset, the openstack block stays
# disabled and the manager runs without a Nova client.
#
# The credentials are NOT passed through `helm --set` (which Tilt echoes to its
# log, leaking OS_PASSWORD). Instead we render the openstack.json ourselves and
# create the Secret via kubectl, feeding the JSON on stdin and suppressing both
# the command echo and its output. The chart is told enabled=true (so it mounts
# the Secret and sets the flag) but createSecret=false (so it does not template
# the Secret from values — we own it here).
os_auth_url = os.getenv('OS_AUTH_URL', '')
openstack_set = ['openstack.enabled=false']
if os_auth_url:
    openstack_set = ['openstack.enabled=true', 'openstack.createSecret=false']

    openstack_json = encode_json({
        'url': os_auth_url,
        'availability': os.getenv('OS_INTERFACE', 'public'),
        'username': os.getenv('OS_USERNAME', ''),
        'password': os.getenv('OS_PASSWORD', ''),
        'projectName': os.getenv('OS_PROJECT_NAME', ''),
        'userDomainName': os.getenv('OS_USER_DOMAIN_NAME', ''),
        'projectDomainName': os.getenv('OS_PROJECT_DOMAIN_NAME', ''),
        'region': os.getenv('OS_REGION_NAME', ''),
    })

    # Pipe the JSON on stdin (never in argv) and apply as a Secret. echo_off
    # hides the command, quiet hides its output, so the password reaches neither
    # the Tilt log nor the process table.
    k8s_yaml(local(
        'kubectl create secret generic vm-operator-openstack ' +
        '--from-file=openstack.json=/dev/stdin ' +
        '--dry-run=client -o yaml',
        stdin=openstack_json,
        echo_off=True,
        quiet=True,
    ))
else:
    warn('OS_AUTH_URL is not set; deploying without OpenStack config. ' +
         'Source your openrc (OS_USERNAME, OS_PASSWORD, ...) before `tilt up` ' +
         'to enable the Nova sync controller.')

chart_yaml = helm(
    'dist',
    name='vm-operator',
    set=[
        'manager.image.repository=' + IMAGE,
        'owner-info.support-group=compute',
    ] + openstack_set,
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
