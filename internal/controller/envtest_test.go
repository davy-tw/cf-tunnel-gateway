package controller

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare/cftest"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
)

const (
	testAccount = "acc1"
	testToken   = "account-token"
	testZone    = "example.com"
	ctrlNS      = "cf-tunnel-gateway-system"
)

type env struct {
	t      *testing.T
	ctx    context.Context
	c      client.Client
	scheme *runtime.Scheme
	cf     *cftest.Server
	zoneID string
	cfg    *config.Config
}

func gatewayAPICRDs(t *testing.T) string {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/gateway-api").Output()
	if err != nil {
		t.Fatalf("locating gateway-api module: %v", err)
	}
	src := filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "standard")
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "vap") {
			continue // not a CRD
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

type envOptions struct {
	env map[string]string
	// noIngressService skips the Ingress and Service reconcilers.
	noIngressService bool
}

func startEnv(t *testing.T, o envOptions) *env {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make test-envtest`")
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))
	te := &envtest.Environment{CRDDirectoryPaths: []string{gatewayAPICRDs(t)}, ErrorIfCRDPathMissing: true}
	restCfg, err := te.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = te.Stop() })

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))

	fake := cftest.New()
	fake.AddToken(testToken, cftest.Token{Account: testAccount, Zones: []string{"*"}})
	zoneID := fake.AddZone(testZone)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	vars := map[string]string{config.EnvAPIURL: srv.URL + "/client/v4", config.EnvNamespace: ctrlNS}
	for k, v := range o.env {
		vars[k] = v
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	cf := cloudflare.New(cfg.APIURL)

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		Client:     ClientOptions(), Cache: CacheOptions(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	c := mgr.GetClient()
	rec := mgr.GetEventRecorder("test")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(NewGatewayClassReconciler(c, cfg, cf).SetupWithManager(mgr))
	must(NewGatewayReconciler(c, scheme, cfg, cf, AllFeatures(), rec, "cluster-uid").SetupWithManager(mgr, nil))
	if !o.noIngressService {
		setupSources(t, mgr, c, scheme, cfg, rec)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, c: direct, scheme: scheme, cf: fake, zoneID: zoneID, cfg: cfg}
	e.create(namespace(ctrlNS), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNS, Name: "cf-tunnel-gateway"},
		Data:       map[string][]byte{KeyAccountID: []byte(testAccount), KeyAPIToken: []byte(testToken)},
	}, gatewayClass("cf-tunnel-gateway"))
	return e
}

func gatewayClass(name string) *gwv1.GatewayClass {
	return &gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: gwv1.GatewayClassSpec{
		ControllerName: gwv1.GatewayController(config.DefaultControllerName),
		ParametersRef: &gwv1.ParametersReference{Group: "", Kind: "Secret", Name: "cf-tunnel-gateway",
			Namespace: ptr.To(gwv1.Namespace(ctrlNS))},
	}}
}

func (e *env) create(objs ...client.Object) {
	e.t.Helper()
	for _, o := range objs {
		if err := e.c.Create(e.ctx, o); err != nil {
			e.t.Fatalf("creating %T %s: %v", o, o.GetName(), err)
		}
	}
}

func (e *env) eventually(what string, f func() error) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("%s: %v", what, err)
}

func (e *env) get(ns, name string, obj client.Object) error {
	return e.c.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)
}

// markReady sets a connector Deployment's ready replicas and connects
// every tunnel in the fake.
func (e *env) markReady(ns, gateway string) {
	e.t.Helper()
	e.eventually("connector deployment", func() error {
		d := &appsv1.Deployment{}
		if err := e.get(ns, connector.DeploymentName(gateway), d); err != nil {
			return err
		}
		d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = 1, 1, 1
		return e.c.Status().Update(e.ctx, d)
	})
	e.cf.ConnectAll()
}

func condIs(conds []metav1.Condition, typ string, status metav1.ConditionStatus) error {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return fmt.Errorf("no %s condition in %+v", typ, conds)
	}
	if c.Status != status {
		return fmt.Errorf("%s = %s (%s: %s)", typ, c.Status, c.Reason, c.Message)
	}
	return nil
}

func namespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func backendService(ns, name string, ports ...int32) *corev1.Service {
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	for _, p := range ports {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("p%d", p), Port: p})
	}
	return s
}

func httpGateway(ns, name string) *gwv1.Gateway {
	return &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: gwv1.GatewaySpec{GatewayClassName: "cf-tunnel-gateway", Listeners: []gwv1.Listener{
			{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80},
		}},
	}
}

func httpRoute(ns, name, gateway, host, backend string, port int32) *gwv1.HTTPRoute {
	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: gwv1.ObjectName(gateway)}}},
			Hostnames:       []gwv1.Hostname{gwv1.Hostname(host)},
			Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
				BackendObjectReference: gwv1.BackendObjectReference{Name: gwv1.ObjectName(backend), Port: ptr.To(gwv1.PortNumber(port))}}}}}},
		},
	}
}

func (e *env) gatewayCond(ns, name, typ string, status metav1.ConditionStatus) func() error {
	return func() error {
		g := &gwv1.Gateway{}
		if err := e.get(ns, name, g); err != nil {
			return err
		}
		return condIs(g.Status.Conditions, typ, status)
	}
}

func (e *env) routeParentCond(ns, name, typ string, status metav1.ConditionStatus) func() error {
	return func() error {
		r := &gwv1.HTTPRoute{}
		if err := e.get(ns, name, r); err != nil {
			return err
		}
		if len(r.Status.Parents) != 1 {
			return fmt.Errorf("parents = %+v", r.Status.Parents)
		}
		return condIs(r.Status.Parents[0].Conditions, typ, status)
	}
}

// record returns the zone's record of a type and name.
func (e *env) record(typ, name string) *cloudflare.DNSRecord {
	for _, r := range e.cf.Records(e.zoneID) {
		if r.Type == typ && r.Name == name {
			return &r
		}
	}
	return nil
}

func TestEnvtestGatewayLifecycle(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.eventually("class accepted", func() error {
		gc := &gwv1.GatewayClass{}
		if err := e.get("", "cf-tunnel-gateway", gc); err != nil {
			return err
		}
		if err := condIs(gc.Status.Conditions, "SupportedVersion", metav1.ConditionTrue); err != nil {
			return err
		}
		return condIs(gc.Status.Conditions, "Accepted", metav1.ConditionTrue)
	})

	e.create(namespace("demo"), backendService("demo", "web", 80), httpGateway("demo", "demo"),
		httpRoute("demo", "web", "demo", "app.example.com", "web", 80))

	var tunnelID string
	e.eventually("tunnel created", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("demo", "demo", g); err != nil {
			return err
		}
		tunnelID = g.Annotations[config.AnnotationTunnel]
		if tunnelID == "" {
			return fmt.Errorf("no tunnel annotation")
		}
		return nil
	})
	tun, _, ok := e.cf.Tunnel(tunnelID)
	if !ok || !strings.HasPrefix(tun.Name, "cf-tunnel-gateway-demo-demo-") {
		t.Fatalf("tunnel = %+v", tun)
	}
	sec := &corev1.Secret{}
	if err := e.get("demo", "demo-cloudflared-token", sec); err != nil || len(sec.Data[connector.TokenKey]) == 0 {
		t.Fatalf("token secret: %v %v", err, sec.Data)
	}
	e.eventually("pending until connected", e.gatewayCond("demo", "demo", "Programmed", metav1.ConditionFalse))

	e.markReady("demo", "demo")
	e.eventually("programmed", e.gatewayCond("demo", "demo", "Programmed", metav1.ConditionTrue))
	e.eventually("route DNS ready", e.routeParentCond("demo", "web", "DNSRecordsReady", metav1.ConditionTrue))

	cfg := e.cf.Config(tunnelID)
	if cfg == nil || len(cfg.Ingress) != 2 || cfg.Ingress[0].Hostname != "app.example.com" ||
		cfg.Ingress[0].Service != "http://web.demo.svc.cluster.local:80" || cfg.Ingress[1].Service != cloudflare.NotFoundService {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.OriginRequest == nil || ptr.Deref(cfg.OriginRequest.KeepAliveConnections, 0) != -1 {
		t.Errorf("origin request = %+v", cfg.OriginRequest)
	}
	cname := e.record("CNAME", "app.example.com")
	if cname == nil || cname.Content != tunnelID+".cfargotunnel.com" || !ptr.Deref(cname.Proxied, false) {
		t.Fatalf("CNAME = %+v", cname)
	}
	if txt := e.record("TXT", "_cf-tunnel-gateway.app.example.com"); txt == nil || !strings.Contains(txt.Content, "gateway=demo/demo") {
		t.Fatalf("TXT = %+v", txt)
	}
	g := &gwv1.Gateway{}
	_ = e.get("demo", "demo", g)
	if len(g.Status.Addresses) != 1 || g.Status.Addresses[0].Value != tunnelID+".cfargotunnel.com" {
		t.Errorf("addresses = %+v", g.Status.Addresses)
	}

	// A record someone else owns is not touched, and reported.
	e.cf.AddRecord(e.zoneID, cloudflare.DNSRecord{Type: "CNAME", Name: "taken.example.com", Content: "elsewhere.example.net"})
	e.create(httpRoute("demo", "taken", "demo", "taken.example.com", "web", 80))
	e.eventually("conflict reported", e.routeParentCond("demo", "taken", "DNSRecordsReady", metav1.ConditionFalse))
	if r := e.record("CNAME", "taken.example.com"); r == nil || r.Content != "elsewhere.example.net" {
		t.Errorf("foreign record changed: %+v", r)
	}

	// A hostname that goes away loses its records.
	r := &gwv1.HTTPRoute{}
	_ = e.get("demo", "web", r)
	r.Spec.Hostnames = []gwv1.Hostname{"other.example.com"}
	if err := e.c.Update(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	e.eventually("old records removed", func() error {
		if e.record("CNAME", "app.example.com") != nil || e.record("TXT", "_cf-tunnel-gateway.app.example.com") != nil {
			return fmt.Errorf("records of app.example.com remain")
		}
		if e.record("CNAME", "other.example.com") == nil {
			return fmt.Errorf("no record for other.example.com")
		}
		return nil
	})

	// Deleting the Gateway deletes the tunnel and its records.
	if err := e.c.Delete(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	e.eventually("gateway gone", func() error {
		err := e.get("demo", "demo", &gwv1.Gateway{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("still there: %v", err)
	})
	if _, _, ok := e.cf.Tunnel(tunnelID); ok {
		if tun, _, _ := e.cf.Tunnel(tunnelID); !tun.Deleted() {
			t.Errorf("tunnel not deleted")
		}
	}
	if e.record("CNAME", "other.example.com") != nil || e.record("TXT", "_cf-tunnel-gateway.other.example.com") != nil {
		t.Errorf("records remain: %+v", e.cf.Records(e.zoneID))
	}
	if e.record("CNAME", "taken.example.com") == nil {
		t.Errorf("the foreign record was deleted")
	}
}
