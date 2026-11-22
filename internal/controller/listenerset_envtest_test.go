package controller

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

func (e *env) listenerSetCond(ns, name, typ string, status metav1.ConditionStatus, reason string) func() error {
	return func() error {
		ls := &gwv1.ListenerSet{}
		if err := e.get(ns, name, ls); err != nil {
			return err
		}
		if err := condIs(ls.Status.Conditions, typ, status); err != nil {
			return err
		}
		for _, c := range ls.Status.Conditions {
			if c.Type == typ && reason != "" && c.Reason != reason {
				return fmt.Errorf("%s reason = %s (%s), want %s", typ, c.Reason, c.Message, reason)
			}
		}
		return nil
	}
}

// routeParentOfKind returns the route's status entry for a parentRef of
// kind.
func routeParentOfKind(r *gwv1.HTTPRoute, kind string) *gwv1.RouteParentStatus {
	for i, p := range r.Status.Parents {
		k := translate.KindGateway
		if p.ParentRef.Kind != nil {
			k = string(*p.ParentRef.Kind)
		}
		if k == kind {
			return &r.Status.Parents[i]
		}
	}
	return nil
}

func (e *env) attachedListenerSets(ns, name string, want int32) func() error {
	return func() error {
		g := &gwv1.Gateway{}
		if err := e.get(ns, name, g); err != nil {
			return err
		}
		if n := ptr.Deref(g.Status.AttachedListenerSets, -1); n != want {
			return fmt.Errorf("attachedListenerSets = %d, want %d", n, want)
		}
		return nil
	}
}

// tunnelRules returns the hostnames in the Gateway's tunnel configuration.
func (e *env) tunnelRules(ns, name string) (string, error) {
	g := &gwv1.Gateway{}
	if err := e.get(ns, name, g); err != nil {
		return "", err
	}
	id := g.Annotations[config.AnnotationTunnel]
	cfg := e.cf.Config(id)
	if cfg == nil {
		return "", fmt.Errorf("tunnel %q has no configuration", id)
	}
	var out []string
	for _, r := range cfg.Ingress {
		out = append(out, r.Hostname+" "+r.Service)
	}
	return strings.Join(out, "\n"), nil
}

func teamListenerSet(name, host string) *gwv1.ListenerSet {
	return &gwv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: name},
		Spec: gwv1.ListenerSetSpec{
			ParentRef: gwv1.ParentGatewayReference{Name: "shared", Namespace: ptr.To(gwv1.Namespace("infra"))},
			Listeners: []gwv1.ListenerEntry{{Name: "web", Protocol: gwv1.HTTPSProtocolType, Port: 443, Hostname: ptr.To(gwv1.Hostname(host))}},
		},
	}
}

func listenerSetRoute(ns, name, set string) *gwv1.HTTPRoute {
	r := httpRoute(ns, name, set, "", "web", 80)
	r.Spec.Hostnames = nil
	r.Spec.ParentRefs[0].Kind = ptr.To(gwv1.Kind(translate.KindListenerSet))
	return r
}

// TestEnvtestListenerSet attaches a ListenerSet from another namespace to
// a Gateway: refused until the Gateway allows it, then published on the
// Gateway's tunnel and DNS, its hostname kept from newer routes, and
// released when it or the Gateway goes away.
func TestEnvtestListenerSet(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	e.create(namespace("infra"), namespace("team"), backendService("team", "web", 80), backendService("infra", "web", 80))
	gw := httpGateway("infra", "shared")
	ls := teamListenerSet("apps", "team.example.com")
	route := listenerSetRoute("team", "app", "apps")
	e.create(gw, ls, route)

	// Gateways allow no ListenerSets by default.
	e.eventually("ListenerSet not allowed", e.listenerSetCond("team", "apps", "Accepted", metav1.ConditionFalse, "NotAllowed"))
	e.eventually("route not allowed", e.routeParentCond("team", "app", "Accepted", metav1.ConditionFalse))
	e.eventually("no ListenerSet attached", e.attachedListenerSets("infra", "shared", 0))

	g := &gwv1.Gateway{}
	if err := e.get("infra", "shared", g); err != nil {
		t.Fatal(err)
	}
	orig := g.DeepCopy()
	g.Spec.AllowedListeners = &gwv1.AllowedListeners{Namespaces: &gwv1.ListenerNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}
	if err := e.c.Patch(e.ctx, g, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	e.eventually("ListenerSet accepted", e.listenerSetCond("team", "apps", "Accepted", metav1.ConditionTrue, "Accepted"))
	e.eventually("ListenerSet waits for its Gateway", e.listenerSetCond("team", "apps", "Programmed", metav1.ConditionFalse, "ParentNotProgrammed"))
	e.markReady("infra", "shared")
	e.eventually("ListenerSet programmed", e.listenerSetCond("team", "apps", "Programmed", metav1.ConditionTrue, "Programmed"))
	e.eventually("attached", e.attachedListenerSets("infra", "shared", 1))
	e.eventually("route published through the ListenerSet", e.routeParentCond("team", "app", "DNSRecordsReady", metav1.ConditionTrue))
	e.eventually("served by the Gateway's tunnel", func() error {
		rules, err := e.tunnelRules("infra", "shared")
		if err != nil {
			return err
		}
		if !strings.Contains(rules, "team.example.com http://web.team.svc") {
			return fmt.Errorf("rules:\n%s", rules)
		}
		return nil
	})
	if g := (&gwv1.Gateway{}); e.get("infra", "shared", g) == nil && len(g.Status.Listeners) != 1 {
		t.Errorf("gateway listeners = %+v", g.Status.Listeners)
	}

	// The owner is recorded where tenants cannot write it.
	e.eventually("owner recorded", func() error {
		g := &gwv1.Gateway{}
		if err := e.get("infra", "shared", g); err != nil {
			return err
		}
		cm := &corev1.ConfigMap{}
		if err := e.get(ctrlNS, recordName(e.cfg, g.UID), cm); err != nil {
			return err
		}
		if v := cm.Data[recordField(config.AnnotationHostnameOwners)]; !strings.Contains(v, `"team.example.com":"ListenerSet team/apps`) {
			return fmt.Errorf("owners = %s", v)
		}
		if v := cm.Data[recordField(config.AnnotationListenerSets)]; v != "team/apps" {
			return fmt.Errorf("listener sets = %q", v)
		}
		return nil
	})

	// A newer route on the Gateway's own listener cannot take it.
	thief := httpRoute("infra", "thief", "shared", "team.example.com", "web", 80)
	e.create(thief)
	e.eventually("newer route conflicted", e.routeParentCond("infra", "thief", "Accepted", metav1.ConditionFalse))
	if err := e.listenerSetCond("team", "apps", "Accepted", metav1.ConditionTrue, "Accepted")(); err != nil {
		t.Errorf("ListenerSet lost its hostname: %v", err)
	}

	// Removing the ListenerSet withdraws its listeners and the route's
	// status entry, and frees the hostname.
	if err := e.c.Delete(e.ctx, ls); err != nil {
		t.Fatal(err)
	}
	e.eventually("stale route status removed", func() error {
		r := &gwv1.HTTPRoute{}
		if err := e.get("team", "app", r); err != nil {
			return err
		}
		if len(r.Status.Parents) != 0 {
			return fmt.Errorf("parents = %+v", r.Status.Parents)
		}
		return nil
	})
	e.eventually("hostname freed", e.routeParentCond("infra", "thief", "Accepted", metav1.ConditionTrue))
	e.eventually("served by the Gateway's route", func() error {
		rules, err := e.tunnelRules("infra", "shared")
		if err != nil {
			return err
		}
		if !strings.Contains(rules, "team.example.com http://web.infra.svc") || strings.Contains(rules, "web.team.svc") {
			return fmt.Errorf("rules:\n%s", rules)
		}
		return nil
	})
	e.eventually("detached", e.attachedListenerSets("infra", "shared", 0))

	// A ListenerSet is released when its Gateway is deleted.
	e.create(teamListenerSet("again", "again.example.com"))
	e.eventually("second ListenerSet accepted", e.listenerSetCond("team", "again", "Accepted", metav1.ConditionTrue, ""))
	if err := e.c.Delete(e.ctx, &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "shared"}}); err != nil {
		t.Fatal(err)
	}
	e.eventually("ListenerSet released", e.listenerSetCond("team", "again", "Accepted", metav1.ConditionFalse, "ParentNotAccepted"))
}

// TestEnvtestListenerSetSelectorAndMove covers allowedListeners with a
// namespace selector (a relabeled namespace is no longer allowed) and a
// ListenerSet whose parentRef moves to a Gateway that does not exist.
func TestEnvtestListenerSetSelectorAndMove(t *testing.T) {
	e := startEnv(t, envOptions{noIngressService: true})
	team := namespace("team")
	team.Labels = map[string]string{"gateway-access": "shared"}
	e.create(namespace("infra"), team, backendService("team", "web", 80))
	gw := httpGateway("infra", "shared")
	gw.Spec.AllowedListeners = &gwv1.AllowedListeners{Namespaces: &gwv1.ListenerNamespaces{
		From:     ptr.To(gwv1.NamespacesFromSelector),
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"gateway-access": "shared"}},
	}}
	e.create(gw, teamListenerSet("apps", "team.example.com"), listenerSetRoute("team", "app", "apps"))
	e.markReady("infra", "shared")
	e.eventually("route published", e.routeParentCond("team", "app", "DNSRecordsReady", metav1.ConditionTrue))

	relabel := func(v string) {
		t.Helper()
		ns := &corev1.Namespace{}
		if err := e.get("", "team", ns); err != nil {
			t.Fatal(err)
		}
		orig := ns.DeepCopy()
		ns.Labels["gateway-access"] = v
		if err := e.c.Patch(e.ctx, ns, client.MergeFrom(orig)); err != nil {
			t.Fatal(err)
		}
	}
	relabel("none")
	e.eventually("ListenerSet no longer allowed", e.listenerSetCond("team", "apps", "Accepted", metav1.ConditionFalse, "NotAllowed"))
	e.eventually("listeners withdrawn", func() error {
		rules, err := e.tunnelRules("infra", "shared")
		if err != nil {
			return err
		}
		if strings.Contains(rules, "team.example.com") {
			return fmt.Errorf("rules:\n%s", rules)
		}
		if r := e.record("CNAME", "team.example.com"); r != nil {
			return fmt.Errorf("CNAME still there: %+v", r)
		}
		return nil
	})
	relabel("shared")
	e.eventually("allowed again", e.routeParentCond("team", "app", "DNSRecordsReady", metav1.ConditionTrue))

	// Move the ListenerSet to a Gateway that does not exist.
	ls := &gwv1.ListenerSet{}
	if err := e.get("team", "apps", ls); err != nil {
		t.Fatal(err)
	}
	orig := ls.DeepCopy()
	ls.Spec.ParentRef.Name = "missing"
	if err := e.c.Patch(e.ctx, ls, client.MergeFrom(orig)); err != nil {
		t.Fatal(err)
	}
	e.eventually("moved ListenerSet released", e.listenerSetCond("team", "apps", "Accepted", metav1.ConditionFalse, "ParentNotAccepted"))
	e.eventually("route status removed", func() error {
		r := &gwv1.HTTPRoute{}
		if err := e.get("team", "app", r); err != nil {
			return err
		}
		if p := routeParentOfKind(r, translate.KindListenerSet); p != nil {
			return fmt.Errorf("parents = %+v", r.Status.Parents)
		}
		return nil
	})
	e.eventually("hostname withdrawn", func() error {
		rules, err := e.tunnelRules("infra", "shared")
		if err != nil {
			return err
		}
		if strings.Contains(rules, "team.example.com") {
			return fmt.Errorf("rules:\n%s", rules)
		}
		return nil
	})
}
