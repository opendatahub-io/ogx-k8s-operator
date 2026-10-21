package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	controllerogx "github.com/ogx-ai/ogx-k8s-operator/ogx-module/internal/controller/ogx"
	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	moduleconfig "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/config"
	configv1 "github.com/openshift/api/config/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = clientgoscheme.Scheme
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
	utilruntime.Must(configv1.Install(scheme))
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
		metricsCertPath   string
	)

	flag.BoolVar(&enableLeaderElect, "leader-elect", enableLeaderElect, "Enable leader election.")
	flag.StringVar(&probeAddr, "health-probe-addr", probeAddr, "The address the probe endpoint binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-addr", metricsAddr, "The address the metrics endpoint binds to.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "", "Directory containing tls.crt and tls.key for HTTPS metrics; defaults to a self-signed certificate.")

	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	if err := run(ctrl.SetupSignalHandler(), cfg, metricsAddr, probeAddr, metricsCertPath, enableLeaderElect); err != nil {
		setupLog.Error(err, "unable to run module operator")
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *moduleconfig.Config, metricsAddr, probeAddr, metricsCertPath string, enableLeaderElect bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	operatorNS := os.Getenv("ODH_MODULE_OPERATOR_NAMESPACE")
	if operatorNS == "" {
		operatorNS = os.Getenv("POD_NAMESPACE")
	}
	leaderNS := operatorNS
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get Kubernetes configuration: %w", err)
	}
	tlsResult, err := setupTLS(ctx, restCfg)
	if err != nil {
		return fmt.Errorf("set up TLS: %w", err)
	}
	metricsOptions, err := buildMetricsOptions(metricsAddr, metricsCertPath, tlsResult.tlsOpts)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
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
		Metrics:                 metricsOptions,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElect,
		LeaderElectionID:        cfg.Controller.LeaderElection.ID,
		LeaderElectionNamespace: leaderNS,
		Cache:                   cache.Options{DefaultNamespaces: watchNamespaces},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := controllerogx.NewReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		cfg,
		dynamicClient,
		discoveryClient,
	).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("create OGX controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("set up ready check: %w", err)
	}
	if tlsResult.hasOpenShiftConfigAPI {
		if err := tlsResult.securityProfileWatcher(mgr.GetClient(), cancel).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("register TLS security profile watcher: %w", err)
		}
	} else if tlsResult.retryTLSConfig {
		if err := mgr.Add(&tlsFallbackWatcher{watcher: tlsResult.securityProfileWatcher(tlsResult.bootstrapClient, cancel)}); err != nil {
			return fmt.Errorf("register TLS fallback watcher: %w", err)
		}
	}

	setupLog.Info("starting manager")
	return mgr.Start(ctx)
}
