package controller

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

func ingressClass() *networkingv1.IngressClass {
	return &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "cf-tunnel-gateway"},
		Spec: networkingv1.IngressClassSpec{Controller: config.IngressControllerName}}
}

func ingress(ns, name, host, svc string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{IngressClassName: ptr.To("cf-tunnel-gateway")}}
	be := networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc, Port: networkingv1.ServiceBackendPort{Number: 80}}}
	if host == "" {
		ing.Spec.DefaultBackend = &be
		return ing
	}
	ing.Spec.Rules = []networkingv1.IngressRule{{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
		Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: ptr.To(networkingv1.PathTypePrefix), Backend: be}},
	}}}}
	return ing
}

// tunnelOf returns the tunnel a Gateway records.
func (e *env) tunnelOf(ns, name string) string {
	g := &gwv1.Gateway{}
	if err := e.get(ns, name, g); err != nil {
		return ""
	}
	return g.Annotations[config.AnnotationTunnel]
}

func (e *env) cnameTarget(host string) string {
	if r := e.record("CNAME", host); r != nil {
		return strings.TrimSuffix(r.Content, ".cfargotunnel.com")
	}
	return ""
}

// collectGarbage deletes what owner owns, standing in for the garbage
// collector, which envtest does not run.
func (e *env) collectGarbage(owner client.Object) {
	e.t.Helper()
	for _, l := range []client.ObjectList{&gwv1.GatewayList{}, &gwv1.HTTPRouteList{}, &gwv1.TCPRouteList{}} {
		if err := e.c.List(e.ctx, l, client.InNamespace(owner.GetNamespace())); err != nil {
			e.t.Fatal(err)
		}
		items, _ := listItems(l)
		for _, o := range items {
			for _, ref := range o.GetOwnerReferences() {
				if ref.UID == owner.GetUID() {
					_ = e.c.Delete(e.ctx, o)
				}
			}
		}
	}
}

func TestEnvtestIngressSharedAndSwitch(t *testing.T) {
	e := startEnv(t, envOptions{env: map[string]string{
		config.EnvBaseDomain: "example.com", config.EnvSwitchGrace: "1s",
	}})
	e.create(ingressClass(), namespace("shop"), backendService("shop", "web", 80),
		ingress("shop", "store", "store.example.com", "web"), ingress("shop", "web", "", "web"))

	shared := config.DefaultInstance
	e.eventually("shared gateway", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("shop", shared, g); err != nil {
			return err
		}
		if g.Annotations[config.AnnotationManaged] != config.ManagedShared {
			return fmt.Errorf("annotations = %v", g.Annotations)
		}
		return nil
	})
	e.markReady("shop", shared)
	e.eventually("ingress status", func() error {
		ing := &networkingv1.Ingress{}
		if err := e.get("shop", "store", ing); err != nil {
			return err
		}
		want := e.tunnelOf("shop", shared) + ".cfargotunnel.com"
		if len(ing.Status.LoadBalancer.Ingress) != 1 || ing.Status.LoadBalancer.Ingress[0].Hostname != want {
			return fmt.Errorf("status = %+v, want %s", ing.Status.LoadBalancer.Ingress, want)
		}
		return nil
	})
	sharedTunnel := e.tunnelOf("shop", shared)
	e.eventually("records on the shared tunnel", func() error {
		for _, h := range []string{"store.example.com", "web-shop.example.com"} {
			if got := e.cnameTarget(h); got != sharedTunnel {
				return fmt.Errorf("%s -> %q, want %s", h, got, sharedTunnel)
			}
		}
		return nil
	})
	rt := &gwv1.HTTPRoute{}
	if err := e.get("shop", "web", rt); err != nil || rt.Annotations[config.AnnotationDefaultHostname] != "web-shop.example.com" {
		t.Fatalf("derived route: %v %v", err, rt.Annotations)
	}

	// Switch the store Ingress to its own tunnel.
	ing := &networkingv1.Ingress{}
	_ = e.get("shop", "store", ing)
	ing.Annotations = map[string]string{config.AnnotationGateway: config.GatewayDedicated}
	if err := e.c.Update(e.ctx, ing); err != nil {
		t.Fatal(err)
	}
	e.eventually("dedicated gateway", func() error {
		if e.tunnelOf("shop", "store") == "" {
			return fmt.Errorf("no tunnel yet")
		}
		return nil
	})
	// The route stays on the shared Gateway until the new one is programmed.
	_ = e.get("shop", "store", rt)
	if len(rt.Spec.ParentRefs) != 1 || string(rt.Spec.ParentRefs[0].Name) != shared {
		t.Fatalf("parents before programmed = %+v", rt.Spec.ParentRefs)
	}
	e.markReady("shop", "store")
	dedicated := e.tunnelOf("shop", "store")
	e.eventually("switched", func() error {
		r := &gwv1.HTTPRoute{}
		if err := e.get("shop", "store", r); err != nil {
			return err
		}
		if len(r.Spec.ParentRefs) != 1 || string(r.Spec.ParentRefs[0].Name) != "store" {
			return fmt.Errorf("parents = %+v", r.Spec.ParentRefs)
		}
		if r.Annotations[config.AnnotationSwitch] != "" {
			return fmt.Errorf("switch annotation remains: %v", r.Annotations)
		}
		if got := e.cnameTarget("store.example.com"); got != dedicated {
			return fmt.Errorf("CNAME -> %q, want %s", got, dedicated)
		}
		if txt := e.record("TXT", "_cf-tunnel-gateway.store.example.com"); txt == nil || !strings.Contains(txt.Content, "gateway=shop/store") {
			return fmt.Errorf("TXT = %+v", txt)
		}
		cfg := e.cf.Config(sharedTunnel)
		for _, rule := range cfg.Ingress {
			if rule.Hostname == "store.example.com" {
				return fmt.Errorf("shared tunnel still serves store.example.com")
			}
		}
		return nil
	})

	// Deleting the Ingress deletes its Gateway, tunnel and records.
	if err := e.c.Delete(e.ctx, ing); err != nil {
		t.Fatal(err)
	}
	e.collectGarbage(ing)
	e.eventually("dedicated cleaned up", func() error {
		if err := e.get("shop", "store", &gwv1.Gateway{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("gateway: %v", err)
		}
		if e.record("CNAME", "store.example.com") != nil {
			return fmt.Errorf("record remains")
		}
		return nil
	})
	// The last user of the shared Gateway goes, and so does the Gateway.
	web := &networkingv1.Ingress{}
	_ = e.get("shop", "web", web)
	if err := e.c.Delete(e.ctx, web); err != nil {
		t.Fatal(err)
	}
	e.collectGarbage(web)
	e.eventually("shared gateway deleted", func() error {
		if err := e.get("shop", shared, &gwv1.Gateway{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("shared gateway: %v", err)
		}
		if e.record("CNAME", "web-shop.example.com") != nil {
			return fmt.Errorf("record remains")
		}
		return nil
	})
}

func TestEnvtestLoadBalancerService(t *testing.T) {
	e := startEnv(t, envOptions{env: map[string]string{config.EnvTCPMode: "true"}})
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "db", Name: "app", Annotations: map[string]string{
			config.AnnotationHostname: "api.example.com", config.AnnotationTCPHostnames: "5432=pg.example.com",
		}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: ptr.To("cf-tunnel-gateway"),
			Ports: []corev1.ServicePort{{Name: "http", Port: 8080, AppProtocol: ptr.To("http")}, {Name: "pg", Port: 5432}, {Name: "extra", Port: 9000}}},
	}
	e.create(namespace("db"), svc)
	shared := config.DefaultInstance
	e.eventually("routes and listeners", func() error {
		if err := e.get("db", "app-http-8080", &gwv1.HTTPRoute{}); err != nil {
			return err
		}
		if err := e.get("db", "app-tcp-5432", &gwv1.TCPRoute{}); err != nil {
			return err
		}
		g := &gwv1.Gateway{}
		if err := e.get("db", shared, g); err != nil {
			return err
		}
		if len(g.Spec.Listeners) != 2 || g.Spec.Listeners[1].Name != TCPListenerName {
			return fmt.Errorf("listeners = %+v", g.Spec.Listeners)
		}
		return nil
	})
	e.markReady("db", shared)
	e.eventually("service status", func() error {
		s := &corev1.Service{}
		if err := e.get("db", "app", s); err != nil {
			return err
		}
		lb := s.Status.LoadBalancer.Ingress
		if len(lb) != 2 || lb[0].Hostname != "api.example.com" || lb[1].Hostname != "pg.example.com" || lb[1].Ports[0].Port != 5432 {
			return fmt.Errorf("status = %+v", lb)
		}
		return nil
	})
	cfg := e.cf.Config(e.tunnelOf("db", shared))
	if cfg == nil || len(cfg.Ingress) != 3 || cfg.Ingress[0].Service != "tcp://app.db.svc.cluster.local:5432" ||
		cfg.Ingress[1].Service != "http://app.db.svc.cluster.local:8080" || cfg.Ingress[2].Service != cloudflare.NotFoundService {
		t.Fatalf("config = %+v", cfg)
	}

	_ = e.get("db", "app", svc)
	if err := e.c.Delete(e.ctx, svc); err != nil {
		t.Fatal(err)
	}
	e.eventually("service gone", func() error {
		if err := e.get("db", "app", &corev1.Service{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("service: %v", err)
		}
		if err := e.get("db", shared, &gwv1.Gateway{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("shared gateway: %v", err)
		}
		return nil
	})
}
