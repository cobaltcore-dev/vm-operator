// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mcl "github.com/cobaltcore-dev/cortex/pkg/multicluster"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
	"github.com/cobaltcore-dev/vm-operator/internal/config"
	"github.com/cobaltcore-dev/vm-operator/internal/nova"
	vmopmcl "github.com/cobaltcore-dev/vm-operator/pkg/multicluster"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// version is the build version stamped into the OpenStack User-Agent. It is
	// left empty by default and set via -ldflags "-X main.version=<sha>" at build
	// time; when empty the User-Agent is just "vm-operator".
	version string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var multiclusterConfigPath string
	var openstackConfigPath string
	var novaSyncInterval time.Duration
	var novaGCInterval time.Duration
	var novaOrphanTTL time.Duration
	var novaDeletionTTL time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":2112",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&multiclusterConfigPath, "multicluster-config", "/etc/vm-operator/multicluster.json",
		"Path to the multicluster client configuration file (mounted from a Secret).")
	flag.StringVar(&openstackConfigPath, "openstack-config", "/etc/vm-operator/openstack/openstack.json",
		"Path to the OpenStack client configuration file (mounted from a Secret).")
	flag.DurationVar(&novaSyncInterval, "nova-sync-interval", nova.DefaultInterval,
		"How often the Nova sync controller polls Nova for changed servers.")
	flag.DurationVar(&novaGCInterval, "nova-gc-interval", nova.DefaultGCInterval,
		"How often the Nova garbage collector re-enqueues all VirtualMachines so a "+
			"deferred prune survives a restart or leader-election failover.")
	flag.DurationVar(&novaOrphanTTL, "nova-orphan-ttl", nova.DefaultOrphanTTL,
		"How long a VirtualMachine carrying no Nova status may exist (measured from "+
			"creation) before the garbage collector prunes it.")
	flag.DurationVar(&novaDeletionTTL, "nova-deletion-ttl", nova.DefaultDeletionTTL,
		"Grace window between the garbage collector marking a VM for deletion (Nova "+
			"reported the server DELETED) and actually deleting it.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Tag every outgoing OpenStack request so operators can identify this
	// client in Nova/Keystone access logs.
	nova.SetUserAgent("vm-operator", version)

	ctx := ctrl.SetupSignalHandler()
	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vm-operator.cobaltcore.cloud",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The home cluster is the cluster the manager runs in. The multicluster
	// client reads/writes VirtualMachines here and, when configured, in remote
	// clusters routed by availability zone.
	homeCluster, err := cluster.New(restConfig, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		setupLog.Error(err, "unable to create home cluster")
		os.Exit(1)
	}
	if err := mgr.Add(homeCluster); err != nil {
		setupLog.Error(err, "unable to add home cluster to manager")
		os.Exit(1)
	}

	vmGVK := schema.GroupVersionKind{
		Group:   v1alpha1.GroupVersion.Group,
		Version: v1alpha1.GroupVersion.Version,
		Kind:    "VirtualMachine",
	}
	multiclusterMonitor := mcl.NewMonitor("vm_operator_")
	multiclusterClient := &mcl.Client{
		HomeCluster:    homeCluster,
		HomeRestConfig: restConfig,
		HomeScheme:     scheme,
		Monitor:        multiclusterMonitor,
		ResourceRouters: map[schema.GroupVersionKind]mcl.ResourceRouter{
			vmGVK: vmopmcl.VirtualMachineRouter{},
		},
	}
	multiclusterConfig, err := config.Load[mcl.ClientConfig](multiclusterConfigPath)
	if err != nil {
		setupLog.Error(err, "unable to load multicluster config", "path", multiclusterConfigPath)
		os.Exit(1)
	}
	if err := multiclusterClient.InitFromConf(ctx, mgr, multiclusterConfig); err != nil {
		setupLog.Error(err, "unable to initialize multicluster client")
		os.Exit(1)
	}
	metrics.Registry.MustRegister(multiclusterMonitor)

	// Nova sync is optional: the Helm chart only mounts the OpenStack config and
	// passes --openstack-config when openstack.enabled=true. When disabled, the
	// flag keeps its default path and the file is simply absent, so we detect that
	// and skip the Nova controllers rather than crashing on a missing config.
	if _, statErr := os.Stat(openstackConfigPath); statErr != nil {
		if os.IsNotExist(statErr) {
			setupLog.Info("openstack config not found, nova sync disabled", "path", openstackConfigPath)
		} else {
			// A permission or other stat error is a real misconfiguration, not an
			// intentional opt-out — fail loudly rather than silently skipping.
			setupLog.Error(statErr, "unable to stat openstack config", "path", openstackConfigPath)
			os.Exit(1)
		}
	} else {
		// Nova metrics: created once, injected into the controllers and the metered
		// OpenStack client, and registered once with the controller-runtime registry
		// (mirroring the multicluster monitor above).
		novaMetrics := nova.NewMetrics()
		metrics.Registry.MustRegister(novaMetrics)

		// Load the OpenStack client configuration (mounted from a Secret) and build
		// the Nova client the sync controller uses.
		openstackConfig, err := config.Load[nova.OpenStackClientConfig](openstackConfigPath)
		if err != nil {
			setupLog.Error(err, "unable to load openstack config", "path", openstackConfigPath)
			os.Exit(1)
		}
		// The region is written to Spec.Region of VirtualMachines created from Nova
		// state, so it is a hard requirement when Nova sync is enabled.
		if openstackConfig.Region == "" {
			setupLog.Error(errors.New("missing region"),
				"openstack region (OS_REGION_NAME) is required when openstack is enabled")
			os.Exit(1)
		}
		openstackClient, err := nova.NewOpenStackClient(ctx, openstackConfig)
		if err != nil {
			setupLog.Error(err, "unable to create openstack client")
			os.Exit(1)
		}
		// Instrument the Nova API calls (count, latency, outcome) via a decorator so
		// the gophercloud client stays metrics-free.
		meteredOpenstackClient := nova.NewMeteredOpenStackClient(openstackClient, novaMetrics)

		if err := (&nova.Controller{
			Client:    multiclusterClient,
			Scheme:    mgr.GetScheme(),
			OpenStack: meteredOpenstackClient,
			Region:    openstackConfig.Region,
			Interval:  novaSyncInterval,
			Metrics:   novaMetrics,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "nova")
			os.Exit(1)
		}

		if err := (&nova.GarbageCollector{
			Client:      multiclusterClient,
			Scheme:      mgr.GetScheme(),
			OrphanTTL:   novaOrphanTTL,
			DeletionTTL: novaDeletionTTL,
			Interval:    novaGCInterval,
			Metrics:     novaMetrics,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "nova-garbagecollector")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
