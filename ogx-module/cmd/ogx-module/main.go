package main

import (
	"flag"
	"os"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	controllerogx "github.com/ogx-ai/ogx-k8s-operator/ogx-module/internal/controller/ogx"
	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	moduleconfig "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/config"
)

var (
	scheme   = clientgoscheme.Scheme
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
}

func main() {
	cfg, err := moduleconfig.Load()
	if err != nil {
		setupLog.Error(err, "unable to load module configuration")
		os.Exit(1)
	}

	var (
		enableLeaderElect = cfg.Controller.LeaderElection.Enabled
		probeAddr         = cfg.Controller.Health.BindAddress
		metricsAddr       = cfg.Controller.Metrics.BindAddress
	)

	flag.BoolVar(&enableLeaderElect, "leader-elect", enableLeaderElect, "Enable leader election.")
	flag.StringVar(&probeAddr, "health-probe-addr", probeAddr, "The address the probe endpoint binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-addr", metricsAddr, "The address the metrics endpoint binds to.")

	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	operatorNS := os.Getenv("ODH_MODULE_OPERATOR_NAMESPACE")
	if operatorNS == "" {
		operatorNS = os.Getenv("POD_NAMESPACE")
	}
	leaderNS := operatorNS
	restCfg := ctrl.GetConfigOrDie()

	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}

	watchNamespaces := map[string]cache.Config{}
	if operatorNS != "" {
		watchNamespaces[operatorNS] = cache.Config{}
	}
	if cfg.ApplicationsNamespace != "" {
		watchNamespaces[cfg.ApplicationsNamespace] = cache.Config{}
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElect,
		LeaderElectionID:        cfg.Controller.LeaderElection.ID,
		LeaderElectionNamespace: leaderNS,
		Cache:                   cache.Options{DefaultNamespaces: watchNamespaces},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := controllerogx.NewReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		cfg,
		dynamicClient,
		discoveryClient,
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ogx")
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
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "unable to run the manager")
		os.Exit(1)
	}
}
