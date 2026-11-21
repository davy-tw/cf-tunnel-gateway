package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// tunnelExists reports whether the fake account still has the tunnel.
func (e *env) tunnelExists(id string) bool {
	for _, t := range e.cf.Tunnels() {
		if t.ID == id {
			return true
		}
	}
	return false
}

// The tunnel and previous-tunnel annotations are only bookkeeping: a
// Gateway author who writes another tunnel's ID into them cannot get that
// tunnel deleted.
func TestEnvtestForgedTunnelAnnotationsDeleteNothing(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	victim := e.cf.AddTunnel(testAccount, "someone-elses-tunnel")
	other := e.cf.AddTunnel(testAccount, "another-tunnel")
	gw := httpGateway("forge", "gw")
	gw.Annotations = map[string]string{config.AnnotationTunnel: victim, config.AnnotationPreviousTunnel: other}
	e.create(namespace("forge"), backendService("forge", "web", 80), gw)

	var own string
	e.eventually("own tunnel created", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("forge", "gw", g); err != nil {
			return err
		}
		if own = g.Annotations[config.AnnotationTunnel]; own == "" || own == victim {
			return fmt.Errorf("tunnel annotation = %q", own)
		}
		return nil
	})
	e.markReady("forge", "gw")
	e.eventually("programmed", e.gatewayCond("forge", "gw", "Programmed", metav1.ConditionTrue))
	if !e.tunnelExists(victim) || !e.tunnelExists(other) {
		t.Fatal("a tunnel named in a forged annotation was deleted while replacing it")
	}

	g := &gwv1.Gateway{}
	if err := e.get("forge", "gw", g); err != nil {
		t.Fatal(err)
	}
	g.Annotations[config.AnnotationTunnel] = victim + "," + own
	g.Annotations[config.AnnotationPreviousTunnel] = other
	if err := e.c.Update(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	e.eventually("gateway gone", func() error {
		if err := e.get("forge", "gw", &gwv1.Gateway{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("still there: %v", err)
		}
		return nil
	})
	if e.tunnelExists(own) {
		t.Error("the Gateway's own tunnel was not deleted")
	}
	if !e.tunnelExists(victim) || !e.tunnelExists(other) {
		t.Error("a tunnel named in a forged annotation was deleted with the Gateway")
	}
}

// Binding an existing tunnel needs the namespace's permission.
func TestEnvtestTunnelIDNeedsNamespacePermission(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	id := e.cf.AddTunnel(testAccount, "prepared")
	gw := httpGateway("bind", "gw")
	gw.Annotations = map[string]string{config.AnnotationTunnelID: id}
	e.create(namespace("bind"), gw)
	e.eventually("not accepted", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("bind", "gw", g); err != nil {
			return err
		}
		if err := condIs(g.Status.Conditions, "Accepted", metav1.ConditionFalse); err != nil {
			return err
		}
		if g.Annotations[config.AnnotationTunnel] != "" {
			return fmt.Errorf("tunnel recorded: %s", g.Annotations[config.AnnotationTunnel])
		}
		return nil
	})
	if c := e.cf.Config(id); c != nil {
		t.Errorf("the unauthorised tunnel was configured: %+v", c)
	}

	ns := &corev1.Namespace{}
	if err := e.get("", "bind", ns); err != nil {
		t.Fatal(err)
	}
	ns.Annotations = map[string]string{config.AnnotationAllowTunnelID: "true"}
	if err := e.c.Update(e.ctx, ns); err != nil {
		t.Fatal(err)
	}
	e.eventually("bound", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("bind", "gw", g); err != nil {
			return err
		}
		if g.Annotations[config.AnnotationTunnel] != id {
			return fmt.Errorf("tunnel = %q", g.Annotations[config.AnnotationTunnel])
		}
		return nil
	})
}

// A hostname stays with the Gateway that published it first, even when
// an older Gateway asks for it later.
func TestEnvtestHostnameStaysWithFirstPublisher(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("a"), namespace("b"), backendService("a", "web", 80), backendService("b", "web", 80),
		httpGateway("a", "old"))
	time.Sleep(1100 * time.Millisecond) // creation timestamps have second precision
	e.create(httpGateway("b", "new"), httpRoute("b", "app", "new", "app.example.com", "web", 80))
	e.markReady("b", "new")
	e.eventually("new publishes", e.routeParentCond("b", "app", "DNSRecordsReady", metav1.ConditionTrue))
	g := &gwv1.Gateway{}
	if err := e.get("b", "new", g); err != nil {
		t.Fatal(err)
	}
	target := g.Annotations[config.AnnotationTunnel] + ".cfargotunnel.com"

	e.create(httpRoute("a", "steal", "old", "app.example.com", "web", 80))
	e.markReady("a", "old")
	e.eventually("older Gateway's route rejected", e.routeParentCond("a", "steal", "Accepted", metav1.ConditionFalse))
	// Give both Gateways a few more reconciles.
	time.Sleep(3 * time.Second)
	if err := e.routeParentCond("b", "app", "Accepted", metav1.ConditionTrue)(); err != nil {
		t.Errorf("first publisher lost its route: %v", err)
	}
	if r := e.record("CNAME", "app.example.com"); r == nil || r.Content != target {
		t.Errorf("CNAME = %+v, want %s", r, target)
	}

	// A restarted controller knows the same from the records.
	fresh := NewGatewayReconciler(e.c, e.scheme, e.cfg, nil, AllFeatures(), nil, "cluster-uid")
	for _, c := range []struct {
		gw, claimant string
	}{{"a/old", "b/new"}, {"b/new", ""}} {
		ns, name, _ := strings.Cut(c.gw, "/")
		g := &gwv1.Gateway{}
		if err := e.get(ns, name, g); err != nil {
			t.Fatal(err)
		}
		claims, err := fresh.claimedHostnames(e.ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		if got := claims["app.example.com"].Gateway; got != c.claimant {
			t.Errorf("after a restart, %s sees app.example.com claimed by %q, want %q", c.gw, got, c.claimant)
		}
	}
}

// A hand-written hostnames annotation claims nothing, even on an accepted
// Gateway whose provisioning never gets to rewrite it.
func TestEnvtestForgedHostnamesClaimNothing(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("x"), namespace("y"), backendService("y", "web", 80),
		// Squatting the token Secret's name keeps x's provisioning pending.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "x", Name: "x-cloudflared-token"}})
	x := httpGateway("x", "x")
	x.Annotations = map[string]string{config.AnnotationHostnames: `{"app.example.com":["HTTPRoute x/r"]}`}
	e.create(x)
	e.eventually("x accepted but not programmed", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("x", "x", g); err != nil {
			return err
		}
		if err := condIs(g.Status.Conditions, "Accepted", metav1.ConditionTrue); err != nil {
			return err
		}
		return condIs(g.Status.Conditions, "Programmed", metav1.ConditionFalse)
	})
	e.create(httpGateway("y", "y"), httpRoute("y", "app", "y", "app.example.com", "web", 80))
	e.markReady("y", "y")
	e.eventually("y publishes", e.routeParentCond("y", "app", "DNSRecordsReady", metav1.ConditionTrue))
}

// A namespace allowed to bind tunnels still cannot get another Gateway's
// tunnel deleted.
func TestEnvtestTunnelIDCannotDeleteOthersTunnel(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("victim"), httpGateway("victim", "gw"))
	var id string
	e.eventually("victim tunnel", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("victim", "gw", g); err != nil {
			return err
		}
		if id = g.Annotations[config.AnnotationTunnel]; id == "" {
			return fmt.Errorf("no tunnel yet")
		}
		return nil
	})
	ns := namespace("attacker")
	ns.Annotations = map[string]string{config.AnnotationAllowTunnelID: "true"}
	gw := httpGateway("attacker", "gw")
	gw.Annotations = map[string]string{config.AnnotationTunnelID: id, config.AnnotationTunnel: id}
	e.create(ns, gw)
	e.eventually("binding refused", e.gatewayCond("attacker", "gw", "Accepted", metav1.ConditionFalse))
	if err := e.c.Delete(e.ctx, gw); err != nil {
		t.Fatal(err)
	}
	e.eventually("attacker gone", func() error {
		if err := e.get("attacker", "gw", &gwv1.Gateway{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("still there: %v", err)
		}
		return nil
	})
	if !e.tunnelExists(id) {
		t.Error("another Gateway's tunnel was deleted")
	}
}

// A Gateway that another installation finalizes is left alone.
func TestEnvtestOtherInstallationsGatewayUntouched(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	id := e.cf.AddTunnel(testAccount, "theirs")
	gw := httpGateway("other", "gw")
	gw.Spec.GatewayClassName = "someone-else"
	gw.Finalizers = []string{config.FinalizerFor("other")}
	gw.Annotations = map[string]string{config.AnnotationTunnel: id}
	e.create(namespace("other"), gw)
	time.Sleep(3 * time.Second)
	g := &gwv1.Gateway{}
	if err := e.get("other", "gw", g); err != nil {
		t.Fatal(err)
	}
	if len(g.Finalizers) != 1 || g.Annotations[config.AnnotationTunnel] != id || !e.tunnelExists(id) {
		t.Errorf("touched: finalizers=%v annotations=%v tunnel exists=%v", g.Finalizers, g.Annotations, e.tunnelExists(id))
	}
}

// Restoring an older copy of a Gateway's controller annotations changes
// nothing: the controller trusts only its own record.
func TestEnvtestReplayedAnnotationsClaimNothing(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("a"), namespace("b"), backendService("a", "web", 80), backendService("b", "web", 80),
		httpGateway("a", "old"), httpRoute("a", "app", "old", "app.example.com", "web", 80))
	e.markReady("a", "old")
	e.eventually("old publishes", e.routeParentCond("a", "app", "DNSRecordsReady", metav1.ConditionTrue))
	g := &gwv1.Gateway{}
	if err := e.get("a", "old", g); err != nil {
		t.Fatal(err)
	}
	saved := map[string]string{}
	for k, v := range g.Annotations {
		saved[k] = v
	}
	if !strings.Contains(saved[config.AnnotationHostnames], "app.example.com") {
		t.Fatalf("hostnames annotation = %q", saved[config.AnnotationHostnames])
	}

	if err := e.c.Delete(e.ctx, httpRoute("a", "app", "old", "", "", 0)); err != nil {
		t.Fatal(err)
	}
	e.eventually("old lets go", func() error {
		if r := e.record("CNAME", "app.example.com"); r != nil {
			return fmt.Errorf("CNAME still there: %+v", r)
		}
		return nil
	})
	time.Sleep(1100 * time.Millisecond) // creation timestamps have second precision
	e.create(httpGateway("b", "new"), httpRoute("b", "app", "new", "app.example.com", "web", 80))
	e.markReady("b", "new")
	e.eventually("new publishes", e.routeParentCond("b", "app", "DNSRecordsReady", metav1.ConditionTrue))
	if err := e.get("b", "new", g); err != nil {
		t.Fatal(err)
	}
	target := g.Annotations[config.AnnotationTunnel] + ".cfargotunnel.com"

	if err := e.get("a", "old", g); err != nil {
		t.Fatal(err)
	}
	g.Annotations = saved
	if err := e.c.Update(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	e.create(httpRoute("a", "steal", "old", "app.example.com", "web", 80))
	e.eventually("replayed claim rejected", e.routeParentCond("a", "steal", "Accepted", metav1.ConditionFalse))
	time.Sleep(3 * time.Second)
	if err := e.routeParentCond("b", "app", "Accepted", metav1.ConditionTrue)(); err != nil {
		t.Errorf("publisher lost its route: %v", err)
	}
	if r := e.record("CNAME", "app.example.com"); r == nil || r.Content != target {
		t.Errorf("CNAME = %+v, want %s", r, target)
	}
}

// A Gateway that is not accepted for a while keeps its hostnames.
func TestEnvtestNotAcceptedKeepsHostnames(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("a"), namespace("b"), backendService("a", "web", 80), backendService("b", "web", 80),
		httpGateway("b", "own"), httpRoute("b", "app", "own", "app.example.com", "web", 80))
	e.markReady("b", "own")
	e.eventually("own publishes", e.routeParentCond("b", "app", "DNSRecordsReady", metav1.ConditionTrue))
	g := &gwv1.Gateway{}
	if err := e.get("b", "own", g); err != nil {
		t.Fatal(err)
	}
	target := g.Annotations[config.AnnotationTunnel] + ".cfargotunnel.com"

	g.Spec.Listeners[0].Port = 8080
	if err := e.c.Update(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	e.eventually("own not accepted", e.gatewayCond("b", "own", "Accepted", metav1.ConditionFalse))
	e.create(httpGateway("a", "other"), httpRoute("a", "steal", "other", "app.example.com", "web", 80))
	e.markReady("a", "other")
	e.eventually("other's route rejected", e.routeParentCond("a", "steal", "Accepted", metav1.ConditionFalse))

	if err := e.get("b", "own", g); err != nil {
		t.Fatal(err)
	}
	g.Spec.Listeners[0].Port = 80
	if err := e.c.Update(e.ctx, g); err != nil {
		t.Fatal(err)
	}
	e.eventually("own publishes again", e.routeParentCond("b", "app", "DNSRecordsReady", metav1.ConditionTrue))
	if r := e.record("CNAME", "app.example.com"); r == nil || r.Content != target {
		t.Errorf("CNAME = %+v, want %s", r, target)
	}
	if err := e.routeParentCond("a", "steal", "Accepted", metav1.ConditionFalse)(); err != nil {
		t.Errorf("other's route: %v", err)
	}
}
