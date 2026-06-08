package main

import (
	"flag"
	"os"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
)

var (
	scheme   = clientgoscheme.Scheme
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		enableLeaderElect bool
		probeAddr         string
	)

	flag.BoolVar(&enableLeaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&probeAddr, "health-probe-addr", ":8081", "The address the probe endpoint binds to.")

	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	leaderNS := os.Getenv("POD_NAMESPACE")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElect,
		LeaderElectionID:        "ogx-module-controller-manager",
		LeaderElectionNamespace: leaderNS,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The reconciler is added in a later task; this bootstrap manager exists so the
	// module subtree can be built and wired independently from the root operator.
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
