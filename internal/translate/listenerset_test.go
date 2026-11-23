package translate

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// listenerSet returns a ListenerSet in ns naming the Gateway default/gw.
func listenerSet(ns, name string, age int, listeners ...gwv1.ListenerEntry) *gwv1.ListenerSet {
	return &gwv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 2, UID: types.UID(ns + "-" + name + "-uid"),
			CreationTimestamp: metav1.NewTime(t0.Add(time.Duration(age) * time.Minute))},
		Spec: gwv1.ListenerSetSpec{
			ParentRef: gwv1.ParentGatewayReference{Name: "gw", Namespace: ptr.To(gwv1.Namespace("default"))},
			Listeners: listeners,
		},
	}
}

func lsEntry(name string, proto gwv1.ProtocolType, port int32, host string) gwv1.ListenerEntry {
	e := gwv1.ListenerEntry{Name: gwv1.SectionName(name), Protocol: proto, Port: gwv1.PortNumber(port),
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}}
	if host != "" {
		e.Hostname = ptr.To(gwv1.Hostname(host))
	}
	return e
}

func allowListenerSets(gw *gwv1.Gateway) *gwv1.Gateway {
	gw.Spec.AllowedListeners = &gwv1.AllowedListeners{Namespaces: &gwv1.ListenerNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}
	return gw
}

// setRoute is a route in ns attached to the ListenerSet ns/set.
func setRoute(ns, name string, age int, set string, hosts []string, rules ...gwv1.HTTPRouteRule) *gwv1.HTTPRoute {
	r := route(name, age, hosts, rules...)
	r.Namespace = ns
	r.Spec.ParentRefs = []gwv1.ParentReference{{Kind: ptr.To(gwv1.Kind(KindListenerSet)), Name: gwv1.ObjectName(set)}}
	return r
}

func setResult(t *testing.T, res *Result, name string) ListenerSetResult {
	t.Helper()
	for _, r := range res.ListenerSets {
		if r.Ref.Name == name {
			return r
		}
	}
	t.Fatalf("ListenerSet %s not in result", name)
	return ListenerSetResult{}
}

func setListenerCond(t *testing.T, res *Result, set, listener, typ string) *metav1.Condition {
	t.Helper()
	for _, l := range setResult(t, res, set).Listeners {
		if string(l.Name) == listener {
			return meta.FindStatusCondition(l.Conditions, typ)
		}
	}
	t.Fatalf("listener %s/%s not in result", set, listener)
	return nil
}

func routeParentCond(t *testing.T, res *Result, name string, i int, typ string) *metav1.Condition {
	t.Helper()
	for _, r := range res.Routes {
		if r.Ref.Name == name {
			if len(r.Parents) <= i {
				t.Fatalf("route %s has %d parent results", name, len(r.Parents))
			}
			return meta.FindStatusCondition(r.Parents[i].Conditions, typ)
		}
	}
	t.Fatalf("route %s not in result", name)
	return nil
}

func TestListenerSetNotAllowedByDefault(t *testing.T) {
	ls := listenerSet("team", "apps", 0, lsEntry("web", gwv1.HTTPProtocolType, 80, "team.example.com"))
	r := setRoute("team", "app", 1, "apps", nil, rule("web"))
	res := Translate(&Input{Gateway: gateway(), ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{r}})
	if s := setResult(t, res, "apps"); s.Accepted || s.Reason != "NotAllowed" || len(s.Listeners) != 0 {
		t.Errorf("ListenerSet: %+v", s)
	}
	if c := parentCond(t, res, "app", "Accepted"); c.Reason != "NotAllowedByListeners" {
		t.Errorf("route: %+v", c)
	}
	if len(res.Config.Ingress) != 1 || res.AttachedListenerSets != 0 {
		t.Errorf("rules: %v, attached %d", rulesOf(res.Config), res.AttachedListenerSets)
	}
	// A Gateway whose listeners all come from ListenerSets is accepted.
	if !res.Accepted {
		t.Errorf("gateway without listeners: %s %s", res.AcceptedReason, res.AcceptedMessage)
	}
}

func TestListenerSetAttach(t *testing.T) {
	gw := allowListenerSets(gateway(gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443}))
	ls := listenerSet("team", "apps", 0,
		lsEntry("web", gwv1.HTTPProtocolType, 80, "team.example.com"),
		lsEntry("same", gwv1.HTTPProtocolType, 80, "same.example.com"))
	ls.Spec.Listeners[1].AllowedRoutes.Namespaces.From = ptr.To(gwv1.NamespacesFromSame)
	app := setRoute("team", "app", 1, "apps", nil, rule("web"))
	app.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("web"))
	// "Same" on a ListenerSet listener is the ListenerSet's namespace.
	other := setRoute("other", "other", 2, "apps", nil, rule("web"))
	other.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("team"))
	other.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("same"))
	// A Gateway parentRef does not reach ListenerSet listeners.
	viaGW := route("via-gw", 3, nil, rule("web"))
	viaGW.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("web"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{app, other, viaGW}})

	if s := setResult(t, res, "apps"); !s.Accepted || s.Reason != "Accepted" || len(s.Listeners) != 2 || s.Listeners[0].AttachedRoutes != 1 {
		t.Errorf("ListenerSet: %+v", s)
	}
	if res.AttachedListenerSets != 1 || len(res.Listeners) != 1 {
		t.Errorf("attached %d, gateway listeners %+v", res.AttachedListenerSets, res.Listeners)
	}
	if c := parentCond(t, res, "app", "Accepted"); c.Status != "True" {
		t.Errorf("app: %+v", c)
	}
	if c := parentCond(t, res, "other", "Accepted"); c.Reason != "NotAllowedByListeners" {
		t.Errorf("other namespace on a Same listener: %+v", c)
	}
	if c := parentCond(t, res, "via-gw", "Accepted"); c.Reason != "NoMatchingParent" {
		t.Errorf("Gateway parentRef with a ListenerSet sectionName: %+v", c)
	}
	got := strings.Join(rulesOf(res.Config), "\n")
	if !strings.Contains(got, "team.example.com http://web.team.svc") {
		t.Errorf("rules:\n%s", got)
	}
	if res.HostOwners["team.example.com"] != "ListenerSet team/apps team-apps-uid" || res.HostOwners["same.example.com"] == "" {
		t.Errorf("owners: %v", res.HostOwners)
	}
}

func TestListenerSetListenerConflicts(t *testing.T) {
	gw := allowListenerSets(gateway(
		gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80},
		gwv1.Listener{Name: "admin", Protocol: gwv1.HTTPSProtocolType, Port: 443, Hostname: ptr.To(gwv1.Hostname("admin.example.com"))},
	))
	older := listenerSet("team", "older", 0, lsEntry("web", gwv1.HTTPSProtocolType, 443, "dup.example.com"))
	ls := listenerSet("team", "apps", 1,
		// The Gateway's hostless HTTP listener holds port 80 without a hostname.
		lsEntry("hostless", gwv1.HTTPProtocolType, 80, ""),
		// Another protocol on port 80.
		lsEntry("tcp", gwv1.TCPProtocolType, 80, ""),
		// The Gateway's hostname, on another port: Cloudflare serves it on both.
		lsEntry("admin", gwv1.HTTPProtocolType, 80, "Admin.Example.com."),
		// An older ListenerSet's hostname.
		lsEntry("dup", gwv1.HTTPSProtocolType, 443, "dup.example.com"),
		lsEntry("port", gwv1.HTTPProtocolType, 8080, "port.example.com"),
		lsEntry("ok", gwv1.HTTPProtocolType, 80, "ok.example.com"),
	)
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls, older}, TCPMode: true, TCPRoute: true})
	want := map[string]string{"hostless": "HostnameConflict", "tcp": "ProtocolConflict", "admin": "HostnameConflict",
		"dup": "HostnameConflict", "port": "PortUnavailable", "ok": "Accepted"}
	for name, reason := range want {
		c := setListenerCond(t, res, "apps", name, "Accepted")
		if c.Reason != reason {
			t.Errorf("%s: %s (%s), want %s", name, c.Reason, c.Message, reason)
		}
		if conflicted := setListenerCond(t, res, "apps", name, "Conflicted"); (conflicted.Status == "True") != strings.HasSuffix(reason, "Conflict") {
			t.Errorf("%s Conflicted: %+v", name, conflicted)
		}
	}
	if s := setResult(t, res, "apps"); !s.Accepted || s.Reason != "ListenersNotValid" {
		t.Errorf("ListenerSet: %+v", s)
	}
	if s := setResult(t, res, "older"); !s.Accepted || res.ListenerSets[0].Ref.Name != "older" {
		t.Errorf("older ListenerSet: %+v", res.ListenerSets)
	}
	for _, l := range setResult(t, res, "apps").Listeners {
		if l.Name == "dup" && l.InvalidReason != "HostnameConflict" {
			t.Errorf("dup invalid reason: %q", l.InvalidReason)
		}
	}
}

// A tenant cannot add a path to a hostname the Gateway's routes publish:
// a longer prefix would take that path's traffic.
func TestListenerSetCannotExtendGatewayHostname(t *testing.T) {
	gw := allowListenerSets(gateway(gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443,
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}}))
	ls := listenerSet("team", "apps", 0, lsEntry("web", gwv1.HTTPProtocolType, 80, ""))
	infra := route("infra", 5, []string{"app.example.com"}, rule("web", prefix("/")))
	// Older than the infra route, and through both parents.
	evil := setRoute("team", "evil", 1, "apps", []string{"app.example.com"}, rule("web", prefix("/admin")))
	evil.Spec.ParentRefs = append(evil.Spec.ParentRefs, gwv1.ParentReference{Name: "gw", Namespace: ptr.To(gwv1.Namespace("default"))})
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{infra, evil}})
	if c := parentCond(t, res, "infra", "Accepted"); c.Status != "True" {
		t.Errorf("infra: %+v", c)
	}
	if c := routeParentCond(t, res, "evil", 0, "Accepted"); c.Reason != ReasonConflicted || !strings.Contains(c.Message, "belongs to Gateway default/gw") {
		t.Errorf("evil through the ListenerSet: %+v", c)
	}
	// Through the Gateway the hostname is the Gateway's, and the older
	// route keeps its path as before.
	if c := routeParentCond(t, res, "evil", 1, "Accepted"); c.Status != "True" {
		t.Errorf("evil through the Gateway: %+v", c)
	}
	for _, rr := range res.Routes {
		if rr.Ref.Name == "evil" && (len(rr.Parents) != 2 || rr.Parents[0].ParentRef.Kind == nil) {
			t.Errorf("parent results out of order: %+v", rr.Parents)
		}
	}
	got := strings.Join(rulesOf(res.Config), "\n")
	if !strings.Contains(got, "app.example.com ^/admin(?:/.*)?$ http://web.team.svc") {
		t.Errorf("rules:\n%s", got)
	}

	// Without the Gateway parentRef nothing of it is published.
	evil.Spec.ParentRefs = evil.Spec.ParentRefs[:1]
	res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{infra, evil}})
	if got := strings.Join(rulesOf(res.Config), "\n"); strings.Contains(got, "/admin") {
		t.Errorf("rules:\n%s", got)
	}
}

// A hostname a ListenerSet listener declared stays with it once recorded:
// a newer route on a hostless Gateway listener cannot take it.
func TestListenerSetDeclaredHostnameIsKept(t *testing.T) {
	gw := allowListenerSets(gateway(gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443,
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}}))
	ls := listenerSet("team", "apps", 0, lsEntry("web", gwv1.HTTPProtocolType, 80, "team.example.com"))
	first := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}})
	if first.HostOwners["team.example.com"] != ListenerSetOwner(ls) {
		t.Fatalf("declared hostname not recorded: %v", first.HostOwners)
	}

	thief := route("thief", 9, []string{"team.example.com"}, rule("web"))
	thief.Namespace = "other"
	thief.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("default"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{thief}, HostOwners: first.HostOwners})
	if c := parentCond(t, res, "thief", "Accepted"); c.Reason != ReasonConflicted || !strings.Contains(c.Message, "ListenerSet team/apps") {
		t.Errorf("thief: %+v", c)
	}
	if c := setListenerCond(t, res, "apps", "web", "Accepted"); c.Status != "True" {
		t.Errorf("listener: %+v", c)
	}
	if res.HostOwners["team.example.com"] != ListenerSetOwner(ls) {
		t.Errorf("owners: %v", res.HostOwners)
	}

	// Without the record (a first reconcile that sees both), the Gateway
	// goes first.
	res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{thief}})
	if c := setListenerCond(t, res, "apps", "web", "Accepted"); c.Reason != "HostnameConflict" {
		t.Errorf("listener without record: %+v", c)
	}
}

// An older ListenerSet that later adds a hostname a newer ListenerSet
// already holds does not take it: the first owner keeps it.
func TestListenerSetEditCannotTakeHostname(t *testing.T) {
	gw := allowListenerSets(gateway())
	a := listenerSet("team-a", "a", 0, lsEntry("a", gwv1.HTTPProtocolType, 80, "a.example.com"))
	b := listenerSet("team-b", "b", 1, lsEntry("b", gwv1.HTTPProtocolType, 80, "b.example.com"))
	first := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{a, b}})
	if first.HostOwners["b.example.com"] != ListenerSetOwner(b) {
		t.Fatalf("owners: %v", first.HostOwners)
	}
	a.Spec.Listeners = append(a.Spec.Listeners, lsEntry("steal", gwv1.HTTPSProtocolType, 443, "b.example.com"))
	a.Generation++
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{a, b}, HostOwners: first.HostOwners})
	if c := setListenerCond(t, res, "a", "steal", "Accepted"); c.Reason != "HostnameConflict" || !strings.Contains(c.Message, "ListenerSet team-b/b") {
		t.Errorf("steal: %+v", c)
	}
	if c := setListenerCond(t, res, "b", "b", "Accepted"); c.Status != "True" {
		t.Errorf("b: %+v", c)
	}

	// A recreated ListenerSet (new UID) is a new owner: the record of the
	// old one no longer counts.
	b2 := b.DeepCopy()
	b2.UID = "recreated"
	b2.CreationTimestamp = metav1.NewTime(t0.Add(time.Hour))
	res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{a, b2}, HostOwners: first.HostOwners})
	if c := setListenerCond(t, res, "a", "steal", "Accepted"); c.Status != "True" {
		t.Errorf("steal after b was recreated: %+v", c)
	}
}

// A wildcard hostname a ListenerSet holds covers the names below it
// against other owners; the Gateway's own listeners may still name them.
func TestListenerSetWildcard(t *testing.T) {
	gw := allowListenerSets(gateway(
		gwv1.Listener{Name: "gw", Protocol: gwv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gwv1.Hostname("www.team.example.com"))},
		gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443,
			AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromAll)}}},
	))
	a := listenerSet("team", "a", 0, lsEntry("wild", gwv1.HTTPProtocolType, 80, "*.team.example.com"))
	b := listenerSet("other", "b", 1, lsEntry("api", gwv1.HTTPSProtocolType, 443, "api.team.example.com"))
	r := route("gw-route", 2, []string{"x.team.example.com"}, rule("web"))
	mine := setRoute("team", "mine", 3, "a", []string{"y.team.example.com"}, rule("web"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{a, b}, HTTPRoutes: []*gwv1.HTTPRoute{r, mine},
		HostOwners: map[string]string{"*.team.example.com": ListenerSetOwner(a)}})
	if c := setListenerCond(t, res, "b", "api", "Accepted"); c.Reason != "HostnameConflict" || !strings.Contains(c.Message, "covered by *.team.example.com") {
		t.Errorf("b: %+v", c)
	}
	if c := parentCond(t, res, "gw-route", "Accepted"); c.Reason != ReasonConflicted {
		t.Errorf("Gateway route below the wildcard: %+v", c)
	}
	if c := parentCond(t, res, "mine", "Accepted"); c.Status != "True" {
		t.Errorf("the wildcard owner's own route: %+v", c)
	}
	if c := setListenerCond(t, res, "a", "wild", "Accepted"); c.Status != "True" {
		t.Errorf("wild: %+v", c)
	}
}

// allowed-hostnames applies to ListenerSet listener hostnames (the
// ListenerSet's namespace) and to routes through a ListenerSet (both
// namespaces).
func TestListenerSetAllowedHostnames(t *testing.T) {
	gw := allowListenerSets(gateway())
	ls := listenerSet("team", "apps", 0,
		lsEntry("ok", gwv1.HTTPProtocolType, 80, "a.team.example.com"),
		lsEntry("bad", gwv1.HTTPSProtocolType, 443, "app.other.org"),
		lsEntry("open", gwv1.HTTPSProtocolType, 443, ""))
	ls.Spec.Listeners[1].AllowedRoutes = nil
	r := setRoute("guest", "r", 1, "apps", []string{"b.team.example.com", "c.guest.example.com"}, rule("web"))
	r.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("team"))
	r.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("open"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{r},
		AllowedHostnames: func(ns string) []string {
			switch ns {
			case "team":
				return []string{"*.team.example.com"}
			case "guest":
				return []string{"*.example.com"}
			}
			return nil
		}})
	if c := setListenerCond(t, res, "apps", "bad", "Accepted"); c.Reason != ReasonHostnameNotAllowed {
		t.Errorf("bad: %+v", c)
	}
	if c := setListenerCond(t, res, "apps", "ok", "Accepted"); c.Status != "True" {
		t.Errorf("ok: %+v", c)
	}
	if c := parentCond(t, res, "r", "PartiallyInvalid"); c == nil || !strings.Contains(c.Message, "c.guest.example.com is not allowed in namespace team") {
		t.Errorf("route: %+v", c)
	}
	got := strings.Join(rulesOf(res.Config), "\n")
	if !strings.Contains(got, "b.team.example.com") || strings.Contains(got, "c.guest") || strings.Contains(got, "other.org") {
		t.Errorf("rules:\n%s", got)
	}
}

// Recorded hostnames of a ListenerSet are released when the Gateway stops
// allowing it, and kept while it is allowed but has no valid listener.
func TestListenerSetRecordedOwners(t *testing.T) {
	ls := listenerSet("team", "apps", 0, lsEntry("web", gwv1.HTTPProtocolType, 80, "team.example.com"))
	owners := map[string]string{"team.example.com": ListenerSetOwner(ls)}
	r := route("gw-route", 5, []string{"team.example.com"}, rule("web"))
	gwListener := gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443}

	// A failed edit: the only listener is not valid.
	broken := ls.DeepCopy()
	broken.Spec.Listeners[0].Port = 8080
	res := Translate(&Input{Gateway: allowListenerSets(gateway(gwListener)), ListenerSets: []*gwv1.ListenerSet{broken},
		HTTPRoutes: []*gwv1.HTTPRoute{r}, HostOwners: owners})
	if c := parentCond(t, res, "gw-route", "Accepted"); c.Reason != ReasonConflicted {
		t.Errorf("route while the ListenerSet is broken: %+v", c)
	}
	if res.HostOwners["team.example.com"] != ListenerSetOwner(ls) {
		t.Errorf("owners while broken: %v", res.HostOwners)
	}

	// No longer allowed: released.
	res = Translate(&Input{Gateway: gateway(gwListener), ListenerSets: []*gwv1.ListenerSet{ls},
		HTTPRoutes: []*gwv1.HTTPRoute{r}, HostOwners: owners})
	if c := parentCond(t, res, "gw-route", "Accepted"); c.Status != "True" {
		t.Errorf("route after the ListenerSet was refused: %+v", c)
	}
	if res.HostOwners["team.example.com"] != OwnerGateway {
		t.Errorf("owners: %v", res.HostOwners)
	}
}

func TestListenerSetTCP(t *testing.T) {
	gw := allowListenerSets(gateway(gwv1.Listener{Name: "tcp", Protocol: gwv1.TCPProtocolType, Port: 1}))
	ls := listenerSet("team", "db", 0, lsEntry("tcp", gwv1.TCPProtocolType, 5432, ""))
	infra := &gwv1.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "infra", CreationTimestamp: metav1.NewTime(t0),
			Annotations: map[string]string{"cf-tunnel-gateway.davy.tw/hostname": "db.example.com"}},
		Spec: gwv1.TCPRouteSpec{CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
			Rules: []gwv1.TCPRouteRule{{BackendRefs: []gwv1.BackendRef{backendRef("pg", 5432).BackendRef}}}},
	}
	team := infra.DeepCopy()
	team.Namespace, team.Name = "team", "team"
	team.Spec.ParentRefs = []gwv1.ParentReference{{Kind: ptr.To(gwv1.Kind(KindListenerSet)), Name: "db"}}
	mine := team.DeepCopy()
	mine.Name = "mine"
	mine.Annotations = map[string]string{"cf-tunnel-gateway.davy.tw/hostname": "team-db.example.com"}
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, TCPRoutes: []*gwv1.TCPRoute{infra, team, mine}, TCPMode: true, TCPRoute: true})
	if c := parentCond(t, res, "team", "Accepted"); c.Reason != ReasonConflicted {
		t.Errorf("team: %+v", c)
	}
	if c := parentCond(t, res, "mine", "Accepted"); c.Status != "True" {
		t.Errorf("mine: %+v", c)
	}
}

// A ListenerSet whose listener breaks while another stays valid keeps the
// broken listener's hostname: an older ListenerSet that was conflicted on
// it does not take it, and the fixed listener is accepted again.
func TestListenerSetPartlyInvalidKeepsHostname(t *testing.T) {
	gw := allowListenerSets(gateway())
	older := listenerSet("tenant-b", "b", 0, lsEntry("x", gwv1.HTTPSProtocolType, 443, "x.example.com"))
	a := listenerSet("tenant-a", "a", 1,
		lsEntry("a", gwv1.HTTPProtocolType, 80, "x.example.com"),
		lsEntry("b", gwv1.HTTPSProtocolType, 443, "y.example.com"))
	owners := map[string]string{"x.example.com": ListenerSetOwner(a), "y.example.com": ListenerSetOwner(a)}
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{older, a}, HostOwners: owners})
	if c := setListenerCond(t, res, "b", "x", "Accepted"); c.Reason != "HostnameConflict" {
		t.Fatalf("older: %+v", c)
	}

	broken := a.DeepCopy()
	broken.Spec.Listeners[0].Port = 8080
	for round := 0; round < 2; round++ {
		res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{older, broken}, HostOwners: owners})
		owners = res.HostOwners
		if c := setListenerCond(t, res, "b", "x", "Accepted"); c.Reason != "HostnameConflict" {
			t.Errorf("round %d: the older ListenerSet took the hostname: %+v", round, c)
		}
	}
	if owners["x.example.com"] != ListenerSetOwner(a) {
		t.Errorf("owners while broken: %v", owners)
	}
	res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{older, a}, HostOwners: owners})
	if c := setListenerCond(t, res, "a", "a", "Accepted"); c.Status != "True" {
		t.Errorf("fixed listener: %+v", c)
	}

	// Once no listener names it, it is released.
	dropped := a.DeepCopy()
	dropped.Spec.Listeners = dropped.Spec.Listeners[1:]
	res = Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{older, dropped}, HostOwners: owners})
	if _, kept := res.HostOwners["x.example.com"]; kept {
		t.Errorf("owners after the listener was removed: %v", res.HostOwners)
	}
}

// A wildcard the Gateway publishes through a route covers the names below
// it against ListenerSets.
func TestListenerSetGatewayWildcard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listener gwv1.Listener
		hosts    []string
	}{
		{"route wildcard", gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}, []string{"*.example.com"}},
		{"listener wildcard", gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gwv1.Hostname("*.example.com"))}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := allowListenerSets(gateway(tc.listener))
			site := route("site", 0, tc.hosts, rule("web"))
			ls := listenerSet("tenant", "evil", 1, lsEntry("admin", gwv1.HTTPSProtocolType, 443, "admin.example.com"),
				lsEntry("open", gwv1.HTTPSProtocolType, 443, ""))
			r := setRoute("tenant", "evil", 2, "evil", []string{"api.example.com"}, rule("evil"))
			r.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("open"))
			res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{site, r}})
			if c := setListenerCond(t, res, "evil", "admin", "Accepted"); c.Reason != "HostnameConflict" || !strings.Contains(c.Message, "covered by *.example.com") {
				t.Errorf("listener: %+v", c)
			}
			if c := parentCond(t, res, "evil", "Accepted"); c.Reason != ReasonConflicted {
				t.Errorf("route: %+v", c)
			}
			if got := strings.Join(rulesOf(res.Config), "\n"); strings.Contains(got, "evil.tenant") {
				t.Errorf("rules:\n%s", got)
			}
		})
	}
	// A Gateway route whose wildcard places no rule publishes nothing and
	// covers nothing.
	{
		gw := allowListenerSets(gateway(gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}))
		broken := route("site", 0, []string{"*.example.com"}, rule("web"))
		broken.Spec.Rules[0].BackendRefs[0].Filters = []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestHeaderModifier}}
		ls := listenerSet("tenant", "ok", 1, lsEntry("admin", gwv1.HTTPSProtocolType, 443, "admin.example.com"))
		res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}, HTTPRoutes: []*gwv1.HTTPRoute{broken}})
		if c := parentCond(t, res, "site", "Accepted"); c.Reason != "UnsupportedValue" {
			t.Fatalf("broken route: %+v", c)
		}
		if c := setListenerCond(t, res, "ok", "admin", "Accepted"); c.Status != "True" {
			t.Errorf("listener under an unpublished Gateway route wildcard: %+v", c)
		}
	}
	// Without a published wildcard, the more specific listener is fine.
	gw := allowListenerSets(gateway(gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gwv1.Hostname("*.example.com"))}))
	ls := listenerSet("tenant", "ok", 1, lsEntry("admin", gwv1.HTTPSProtocolType, 443, "admin.example.com"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls}})
	if c := setListenerCond(t, res, "ok", "admin", "Accepted"); c.Status != "True" {
		t.Errorf("listener under an unused Gateway wildcard listener: %+v", c)
	}
}

// A ListenerSet listener hostname another Gateway publishes is a conflict.
func TestListenerSetHostnameClaimedElsewhere(t *testing.T) {
	gw := allowListenerSets(gateway())
	ls := listenerSet("tenant", "a", 0, lsEntry("x", gwv1.HTTPSProtocolType, 443, "x.example.com"))
	res := Translate(&Input{Gateway: gw, ListenerSets: []*gwv1.ListenerSet{ls},
		Claimed: map[string]Claim{"x.example.com": {Gateway: "other/gw"}}})
	if c := setListenerCond(t, res, "a", "x", "Accepted"); c.Reason != "HostnameConflict" || !strings.Contains(c.Message, "other/gw") {
		t.Errorf("listener: %+v", c)
	}
	if _, ok := res.HostOwners["x.example.com"]; ok {
		t.Errorf("owners: %v", res.HostOwners)
	}
}
