// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"

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

	"github.com/cobaltcore-dev/cortex/pkg/multicluster"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
	"github.com/cobaltcore-dev/vm-operator/internal"
	"github.com/cobaltcore-dev/vm-operator/internal/nova"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
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

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":2112",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&multiclusterConfigPath, "multicluster-config", "/etc/vm-operator/multicluster.json",
		"Path to the multicluster client configuration file (mounted from a Secret).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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
	multiclusterMonitor := multicluster.NewMonitor("vm_operator_")
	multiclusterClient := &multicluster.Client{
		HomeCluster:    homeCluster,
		HomeRestConfig: restConfig,
		HomeScheme:     scheme,
		Monitor:        multiclusterMonitor,
		ResourceRouters: map[schema.GroupVersionKind]multicluster.ResourceRouter{
			vmGVK: internal.VirtualMachineRouter{},
		},
	}
	multiclusterConfig, err := internal.LoadClientConfig(multiclusterConfigPath)
	if err != nil {
		setupLog.Error(err, "unable to load multicluster config", "path", multiclusterConfigPath)
		os.Exit(1)
	}
	if err := multiclusterClient.InitFromConf(ctx, mgr, multiclusterConfig); err != nil {
		setupLog.Error(err, "unable to initialize multicluster client")
		os.Exit(1)
	}
	metrics.Registry.MustRegister(multiclusterMonitor)

	if err := (&nova.Controller{
		Client: multiclusterClient,
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "nova")
		os.Exit(1)
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
