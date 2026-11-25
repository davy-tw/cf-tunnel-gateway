// Command manager runs the cf-tunnel-gateway controller.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/controller"
)

func main() {
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	fail := func(err error, msg string) {
		setupLog.Error(err, msg)
		os.Exit(1)
	}

	cfg, err := config.FromEnv()
	if err != nil {
		fail(err, "invalid configuration")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))

	restCfg := ctrl.GetConfigOrDie()
	dc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		fail(err, "creating discovery client")
	}
	// Wait (without crash-looping) until the Gateway API CRDs are installed.
	// Meanwhile the probe address answers /healthz, so that the liveness
	// probe does not restart the container, and fails /readyz.
	waitProbes := startWaitingProbes(cfg.ProbeAddr)
	features := &controller.Features{}
	for {
		grpc, tcp, grant, ls, err := controller.DetectFeatures(dc)
		if err == nil {
			features.Set(grpc, tcp, grant, ls)
			break
		}
		var missing *controller.ErrMissingCoreCRDs
		if errors.As(err, &missing) {
			setupLog.Error(err, "waiting for Gateway API CRDs; retrying in 30s")
		} else {
			setupLog.Error(err, "discovering Gateway API; retrying in 30s")
		}
		time.Sleep(30 * time.Second)
	}
	if err := waitProbes.Shutdown(context.Background()); err != nil {
		setupLog.Error(err, "stopping the startup probe server")
	}
	for kind, on := range map[string]bool{
		"GRPCRoute": features.GRPCRoute(), "TCPRoute": features.TCPRoute(), "ReferenceGrant": features.ReferenceGrant(),
		"ListenerSet": features.ListenerSet(),
	} {
		if !on {
			setupLog.Info("optional Gateway API kind not served at v1; feature disabled until it is installed", "kind", kind)
		}
	}

	// The kube-system namespace UID identifies the cluster in tunnel names.
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		fail(err, "creating client")
	}
	ks := &corev1.Namespace{}
	if err := direct.Get(context.Background(), types.NamespacedName{Name: "kube-system"}, ks); err != nil {
		fail(err, "reading the kube-system namespace (its UID identifies the cluster)")
	}
	controller.CheckIngressClass(context.Background(), direct, cfg)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:  cfg.ProbeAddr,
		LeaderElection:          true,
		LeaderElectionID:        cfg.Instance,
		LeaderElectionNamespace: cfg.Namespace,
		Client:                  controller.ClientOptions(),
		Cache:                   controller.CacheOptions(cfg),
	})
	if err != nil {
		fail(err, "creating manager")
	}
	c := mgr.GetClient()
	cf := cloudflare.New(cfg.APIURL)
	rec := mgr.GetEventRecorder(cfg.Instance)
	fw := &controller.FeatureWatcher{Discovery: dc, Features: features}

	if err := controller.NewGatewayClassReconciler(c, cfg, cf).SetupWithManager(mgr); err != nil {
		fail(err, "setting up GatewayClass controller")
	}
	if err := controller.NewGatewayReconciler(c, scheme, cfg, cf, features, rec, string(ks.UID)).SetupWithManager(mgr, fw); err != nil {
		fail(err, "setting up Gateway controller")
	}
	if cfg.IngressEnabled || cfg.ServiceEnabled {
		if err := controller.NewSharedGatewayReconciler(c, scheme, cfg, cf, features, rec).SetupWithManager(mgr); err != nil {
			fail(err, "setting up shared Gateway controller")
		}
	}
	if cfg.IngressEnabled {
		if err := controller.NewIngressReconciler(c, scheme, cfg, cf, features, rec).SetupWithManager(mgr); err != nil {
			fail(err, "setting up Ingress controller")
		}
	}
	if cfg.ServiceEnabled {
		if err := controller.NewServiceReconciler(c, scheme, cfg, cf, features, rec).SetupWithManager(mgr); err != nil {
			fail(err, "setting up Service controller")
		}
	}
	for _, r := range []interface{ Start(context.Context) error }{
		fw, &controller.FrontDoorCleanup{Client: c, Config: cfg, Features: features},
	} {
		if err := mgr.Add(runnable{r}); err != nil {
			fail(err, "adding runnable")
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		fail(err, "adding health check")
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		fail(err, "adding ready check")
	}
	setupLog.Info("starting", "instance", cfg.Instance, "controllerName", cfg.ControllerName,
		"ingress", cfg.IngressEnabled, "loadBalancer", cfg.ServiceEnabled, "sharedScope", cfg.SharedScope,
		"accountCredentials", cfg.HasAccountCredentials())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fail(err, "manager exited")
	}
}

// runnable runs as the leader.
type runnable struct {
	r interface{ Start(context.Context) error }
}

func (r runnable) Start(ctx context.Context) error { return r.r.Start(ctx) }
func (r runnable) NeedLeaderElection() bool        { return true }

// startWaitingProbes serves the health probes until the manager, which
// serves them afterwards, starts.
func startWaitingProbes(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "waiting for the Gateway API CRDs", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ctrl.Log.WithName("setup").Error(err, "startup probe server")
		}
	}()
	return srv
}
