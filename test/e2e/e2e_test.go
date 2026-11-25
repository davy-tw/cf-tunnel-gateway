//go:build e2e

// Package e2e runs against a real cluster (the current kubeconfig context)
// with the Gateway API CRDs, cf-tunnel-gateway and the fake Cloudflare API
// installed (`make e2e-install`):
//
//	go test -tags e2e ./test/e2e/ -v
//
// It checks what needs a real cluster rather than envtest: connector pods
// admitted under the restricted Pod Security Standard and started by a
// real kubelet, Gateway, route, Ingress and Service translation against
// the fake API, and deletion with real garbage collection. The connectors
// cannot reach Cloudflare, so the Gateways that must become Programmed run
// without a connector (infrastructure parameter disableConnector).
//
// Settings (environment variables):
//
//   - E2E_GATEWAY_CLASS: the installed GatewayClass (default
//     cf-tunnel-gateway).
//   - E2E_NAMESPACE: where the controller and the fake API run (default
//     cf-tunnel-gateway-system).
//   - E2E_SHARED_PARAMETERS: the shared Gateway parameters ConfigMap name
//     the controller was installed with (default e2e-connector).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare/cftest"
)

const (
	domain           = "cf-tunnel-gateway.davy.tw"
	annotationTunnel = domain + "/tunnel"
)

type suite struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	cs  *kubernetes.Clientset
	ns  string
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// setup connects to the cluster and creates a namespace for the test that
// enforces the restricted Pod Security Standard, with the shared Gateway
// parameters ConfigMap turning the connector off.
func setup(t *testing.T, prefix string) *suite {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gwv1.Install(scheme))
	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{t: t, ctx: context.Background(), c: c, cs: cs, ns: fmt.Sprintf("%s-%d", prefix, time.Now().Unix())}
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns, Labels: map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted",
	}}}, s.connectorParams(env("E2E_SHARED_PARAMETERS", "e2e-connector"), true))
	t.Cleanup(func() {
		if t.Failed() {
			s.dump()
		}
		_ = c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.ns}})
	})
	return s
}

func (s *suite) connectorParams(name string, disabled bool) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: name},
		Data: map[string]string{"disableConnector": fmt.Sprint(disabled)}}
}

func (s *suite) create(objs ...client.Object) {
	s.t.Helper()
	for _, o := range objs {
		if err := s.c.Create(s.ctx, o); err != nil {
			s.t.Fatalf("creating %T %s: %v", o, o.GetName(), err)
		}
	}
}

func (s *suite) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: s.ns, Name: name}
}

func (s *suite) wait(what string, timeout time.Duration, f func() error) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	s.t.Fatalf("%s: %v", what, err)
}

func (s *suite) gone(obj client.Object, name string) func() error {
	return func() error {
		if err := s.c.Get(s.ctx, s.key(name), obj); !apierrors.IsNotFound(err) {
			return fmt.Errorf("%T %s still there (%v)", obj, name, err)
		}
		return nil
	}
}

// state reads the fake Cloudflare API's state through the API server's
// service proxy.
func (s *suite) state() (*cftest.State, error) {
	raw, err := s.cs.CoreV1().Services(env("E2E_NAMESPACE", "cf-tunnel-gateway-system")).
		ProxyGet("http", "fake-cloudflare-api", "8080", cftest.StatePath, nil).DoRaw(s.ctx)
	if err != nil {
		return nil, err
	}
	st := &cftest.State{}
	return st, json.Unmarshal(raw, st)
}

func (s *suite) cname(st *cftest.State, host string) string {
	for _, r := range st.Records {
		if r.Type == "CNAME" && r.Name == host {
			return strings.TrimSuffix(r.Content, ".cfargotunnel.com")
		}
	}
	return ""
}

func (s *suite) programmed(name string) func() error {
	return func() error {
		gw := &gwv1.Gateway{}
		if err := s.c.Get(s.ctx, s.key(name), gw); err != nil {
			return err
		}
		c := meta.FindStatusCondition(gw.Status.Conditions, "Programmed")
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("Programmed: %+v", c)
		}
		return nil
	}
}

func (s *suite) tunnelOf(name string) string {
	gw := &gwv1.Gateway{}
	if err := s.c.Get(s.ctx, s.key(name), gw); err != nil {
		return ""
	}
	return gw.Annotations[annotationTunnel]
}

// dump logs the test namespace's Gateways, pods and events to explain a
// failure.
func (s *suite) dump() {
	var gws gwv1.GatewayList
	if err := s.c.List(s.ctx, &gws, client.InNamespace(s.ns)); err == nil {
		for _, gw := range gws.Items {
			s.t.Logf("Gateway %s: annotations=%v deleting=%v conditions=%+v", gw.Name, gw.Annotations, gw.DeletionTimestamp != nil, gw.Status.Conditions)
		}
	}
	var pods corev1.PodList
	if err := s.c.List(s.ctx, &pods, client.InNamespace(s.ns)); err == nil {
		for _, p := range pods.Items {
			s.t.Logf("Pod %s: phase=%s containers=%+v", p.Name, p.Status.Phase, p.Status.ContainerStatuses)
		}
	}
	var evs corev1.EventList
	if err := s.c.List(s.ctx, &evs, client.InNamespace(s.ns)); err == nil {
		for _, e := range evs.Items {
			s.t.Logf("Event %s %s/%s: %s %s", e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
		}
	}
	if st, err := s.state(); err == nil {
		b, _ := json.MarshalIndent(st, "", "  ")
		s.t.Logf("fake Cloudflare state: %s", b)
	}
}

func backend(ns, name string, port int32) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": name}, Ports: []corev1.ServicePort{{Name: "http", Port: port}},
	}}
}

// TestGateway covers a user Gateway: the connector starts under the
// restricted Pod Security Standard, and with the connector off the
// Gateway is programmed, publishes its route and cleans up on deletion.
func TestGateway(t *testing.T) {
	s := setup(t, "gw")
	class := gwv1.ObjectName(env("E2E_GATEWAY_CLASS", "cf-tunnel-gateway"))
	s.create(backend(s.ns, "web", 80), s.connectorParams("off", true),
		&gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "with-connector"}, Spec: gwv1.GatewaySpec{
			GatewayClassName: class, Listeners: []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}},
		}},
		&gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "demo"}, Spec: gwv1.GatewaySpec{
			GatewayClassName: class, Listeners: []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}},
			Infrastructure: &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{Kind: "ConfigMap", Name: "off"}},
		}},
		&gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "web"}, Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "demo"}}},
			Hostnames:       []gwv1.Hostname{gwv1.Hostname(s.ns + ".example.com")},
			Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
				BackendObjectReference: gwv1.BackendObjectReference{Name: "web", Port: ptr.To(gwv1.PortNumber(80))}}}}}},
		}},
	)

	// The connector pods are admitted and scheduled (they cannot connect
	// to the fake API's tunnels, so they never become ready).
	s.wait("connector pod created", 3*time.Minute, func() error {
		var pods corev1.PodList
		if err := s.c.List(s.ctx, &pods, client.InNamespace(s.ns), client.MatchingLabels{"app.kubernetes.io/name": "cloudflared"}); err != nil {
			return err
		}
		for _, p := range pods.Items {
			for _, cs := range p.Status.ContainerStatuses {
				if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
					return nil
				}
			}
		}
		return fmt.Errorf("no started connector container among %d pods", len(pods.Items))
	})

	s.wait("gateway programmed", 2*time.Minute, s.programmed("demo"))
	tunnel := s.tunnelOf("demo")
	host := s.ns + ".example.com"
	s.wait("published", time.Minute, func() error {
		st, err := s.state()
		if err != nil {
			return err
		}
		cfg := st.Configs[tunnel]
		if cfg == nil || len(cfg.Ingress) == 0 || cfg.Ingress[0].Hostname != host ||
			cfg.Ingress[0].Service != fmt.Sprintf("http://web.%s.svc.cluster.local:80", s.ns) {
			return fmt.Errorf("config = %+v", cfg)
		}
		if got := s.cname(st, host); got != tunnel {
			return fmt.Errorf("CNAME %s -> %q", host, got)
		}
		return nil
	})

	// Deletion removes the tunnel and its records.
	if err := s.c.Delete(s.ctx, &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "demo"}}); err != nil {
		t.Fatal(err)
	}
	s.wait("gateway deleted", 2*time.Minute, s.gone(&gwv1.Gateway{}, "demo"))
	st, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	for _, tun := range st.Tunnels {
		if tun.ID == tunnel {
			t.Errorf("tunnel %s not deleted", tunnel)
		}
	}
	if s.cname(st, host) != "" {
		t.Errorf("CNAME of %s not deleted", host)
	}
}

// TestIngressAndService covers the front doors on the namespace's shared
// Gateway, a switch to a dedicated Gateway, and cleanup through garbage
// collection.
func TestIngressAndService(t *testing.T) {
	s := setup(t, "fd")
	shared := env("E2E_SHARED_GATEWAY", "cf-tunnel-gateway")
	host := "site-" + s.ns + ".example.com"
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "site"}, Spec: networkingv1.IngressSpec{
		IngressClassName: ptr.To(env("E2E_INGRESS_CLASS", "cf-tunnel-gateway")),
		Rules: []networkingv1.IngressRule{{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: ptr.To(networkingv1.PathTypePrefix), Backend: networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}}},
		}}}},
	}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "db", Annotations: map[string]string{
		domain + "/tcp-hostnames": "5432",
	}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: ptr.To(env("E2E_LOAD_BALANCER_CLASS", "cf-tunnel-gateway")),
		Selector: map[string]string{"app": "db"}, Ports: []corev1.ServicePort{{Name: "pg", Port: 5432}}}}
	s.create(backend(s.ns, "web", 80), ing, svc)

	s.wait("shared gateway programmed", 2*time.Minute, s.programmed(shared))
	sharedTunnel := s.tunnelOf(shared)
	dbHost := "db-" + s.ns + "-5432.example.com"
	s.wait("statuses", time.Minute, func() error {
		i := &networkingv1.Ingress{}
		if err := s.c.Get(s.ctx, s.key("site"), i); err != nil {
			return err
		}
		if lb := i.Status.LoadBalancer.Ingress; len(lb) != 1 || lb[0].Hostname != sharedTunnel+".cfargotunnel.com" {
			return fmt.Errorf("ingress status = %+v", lb)
		}
		v := &corev1.Service{}
		if err := s.c.Get(s.ctx, s.key("db"), v); err != nil {
			return err
		}
		if lb := v.Status.LoadBalancer.Ingress; len(lb) != 1 || lb[0].Hostname != dbHost {
			return fmt.Errorf("service status = %+v", lb)
		}
		return nil
	})
	s.wait("published on the shared tunnel", time.Minute, func() error {
		st, err := s.state()
		if err != nil {
			return err
		}
		for _, h := range []string{host, dbHost} {
			if got := s.cname(st, h); got != sharedTunnel {
				return fmt.Errorf("CNAME %s -> %q", h, got)
			}
		}
		return nil
	})

	// Switch the Ingress to a dedicated Gateway without a connector.
	s.create(s.connectorParams("site-connector", true))
	if err := s.c.Get(s.ctx, s.key("site"), ing); err != nil {
		t.Fatal(err)
	}
	ing.Annotations = map[string]string{domain + "/gateway": "dedicated", domain + "/parameters": "site-connector"}
	if err := s.c.Update(s.ctx, ing); err != nil {
		t.Fatal(err)
	}
	s.wait("dedicated gateway programmed", 2*time.Minute, s.programmed("site"))
	dedicated := s.tunnelOf("site")
	s.wait("switched", 2*time.Minute, func() error {
		r := &gwv1.HTTPRoute{}
		if err := s.c.Get(s.ctx, s.key("site"), r); err != nil {
			return err
		}
		if len(r.Spec.ParentRefs) != 1 || r.Spec.ParentRefs[0].Name != "site" {
			return fmt.Errorf("parents = %+v", r.Spec.ParentRefs)
		}
		st, err := s.state()
		if err != nil {
			return err
		}
		if got := s.cname(st, host); got != dedicated {
			return fmt.Errorf("CNAME %s -> %q, want %s", host, got, dedicated)
		}
		return nil
	})

	// Deleting both lets garbage collection take the routes and the
	// dedicated Gateway, and the unused shared Gateway goes too.
	if err := s.c.Delete(s.ctx, ing); err != nil {
		t.Fatal(err)
	}
	if err := s.c.Delete(s.ctx, svc); err != nil {
		t.Fatal(err)
	}
	s.wait("dedicated gateway deleted", 2*time.Minute, s.gone(&gwv1.Gateway{}, "site"))
	s.wait("shared gateway deleted", 2*time.Minute, s.gone(&gwv1.Gateway{}, shared))
	s.wait("service deleted", time.Minute, s.gone(&corev1.Service{}, "db"))
	st, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{host, dbHost} {
		if s.cname(st, h) != "" {
			t.Errorf("CNAME of %s not deleted", h)
		}
	}
}

// TestListenerSet covers a ListenerSet from another namespace: its
// listener is served on its parent Gateway's tunnel and DNS once the
// Gateway allows it, and a newer route on the Gateway cannot take its
// hostname.
func TestListenerSet(t *testing.T) {
	s := setup(t, "ls")
	class := gwv1.ObjectName(env("E2E_GATEWAY_CLASS", "cf-tunnel-gateway"))
	team := s.ns + "-team"
	s.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: team}})
	t.Cleanup(func() {
		_ = s.c.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: team}})
	})
	host := team + ".example.com"
	s.create(backend(s.ns, "web", 80), backend(team, "web", 80), s.connectorParams("off", true),
		&gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "shared"}, Spec: gwv1.GatewaySpec{
			GatewayClassName: class, Listeners: []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}},
			AllowedListeners: &gwv1.AllowedListeners{Namespaces: &gwv1.ListenerNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}},
			Infrastructure:   &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{Kind: "ConfigMap", Name: "off"}},
		}},
		&gwv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Namespace: team, Name: "apps"}, Spec: gwv1.ListenerSetSpec{
			ParentRef: gwv1.ParentGatewayReference{Name: gwv1.ObjectName("shared"), Namespace: ptr.To(gwv1.Namespace(s.ns))},
			Listeners: []gwv1.ListenerEntry{{Name: "web", Protocol: gwv1.HTTPSProtocolType, Port: 443, Hostname: ptr.To(gwv1.Hostname(host))}},
		}},
		&gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: team, Name: "app"}, Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Kind: ptr.To(gwv1.Kind("ListenerSet")), Name: "apps"}}},
			Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
				BackendObjectReference: gwv1.BackendObjectReference{Name: "web", Port: ptr.To(gwv1.PortNumber(80))}}}}}},
		}},
	)
	s.wait("gateway programmed", 2*time.Minute, s.programmed("shared"))
	s.wait("ListenerSet programmed", time.Minute, func() error {
		ls := &gwv1.ListenerSet{}
		if err := s.c.Get(s.ctx, types.NamespacedName{Namespace: team, Name: "apps"}, ls); err != nil {
			return err
		}
		c := meta.FindStatusCondition(ls.Status.Conditions, "Programmed")
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("Programmed: %+v", c)
		}
		return nil
	})
	tunnel := s.tunnelOf("shared")
	s.wait("published", time.Minute, func() error {
		st, err := s.state()
		if err != nil {
			return err
		}
		cfg := st.Configs[tunnel]
		if cfg == nil || len(cfg.Ingress) == 0 || cfg.Ingress[0].Hostname != host ||
			cfg.Ingress[0].Service != fmt.Sprintf("http://web.%s.svc.cluster.local:80", team) {
			return fmt.Errorf("config = %+v", cfg)
		}
		if got := s.cname(st, host); got != tunnel {
			return fmt.Errorf("CNAME %s -> %q", host, got)
		}
		return nil
	})

	// A newer route on the Gateway cannot take the ListenerSet's hostname.
	s.create(&gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: s.ns, Name: "thief"}, Spec: gwv1.HTTPRouteSpec{
		CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "shared"}}},
		Hostnames:       []gwv1.Hostname{gwv1.Hostname(host)},
		Rules: []gwv1.HTTPRouteRule{{BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
			BackendObjectReference: gwv1.BackendObjectReference{Name: "web", Port: ptr.To(gwv1.PortNumber(80))}}}}}},
	}})
	s.wait("newer route conflicted", time.Minute, func() error {
		r := &gwv1.HTTPRoute{}
		if err := s.c.Get(s.ctx, s.key("thief"), r); err != nil {
			return err
		}
		if len(r.Status.Parents) != 1 {
			return fmt.Errorf("parents = %+v", r.Status.Parents)
		}
		c := meta.FindStatusCondition(r.Status.Parents[0].Conditions, "Accepted")
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Conflicted" {
			return fmt.Errorf("Accepted: %+v", c)
		}
		return nil
	})
	st, err := s.state()
	if err != nil {
		t.Fatal(err)
	}
	if cfg := st.Configs[tunnel]; cfg == nil || cfg.Ingress[0].Service != fmt.Sprintf("http://web.%s.svc.cluster.local:80", team) {
		t.Errorf("config after the newer route = %+v", cfg)
	}
}
