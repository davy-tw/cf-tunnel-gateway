package translate

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func gateway(listeners ...gwv1.Listener) *gwv1.Gateway {
	return &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 3},
		Spec:       gwv1.GatewaySpec{GatewayClassName: "cf", Listeners: listeners},
	}
}

func httpListener() gwv1.Listener {
	return gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}
}

func svc(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Ports: ports}}
}

func services(list ...*corev1.Service) func(ns, name string) *corev1.Service {
	return func(ns, name string) *corev1.Service {
		for _, s := range list {
			if s.Namespace == ns && s.Name == name {
				return s
			}
		}
		return nil
	}
}

func backendRef(name string, port int32) gwv1.HTTPBackendRef {
	return gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
		Name: gwv1.ObjectName(name), Port: ptr.To(gwv1.PortNumber(port))}}}
}

func prefix(p string) gwv1.HTTPRouteMatch {
	return gwv1.HTTPRouteMatch{Path: &gwv1.HTTPPathMatch{Type: ptr.To(gwv1.PathMatchPathPrefix), Value: ptr.To(p)}}
}

func exact(p string) gwv1.HTTPRouteMatch {
	return gwv1.HTTPRouteMatch{Path: &gwv1.HTTPPathMatch{Type: ptr.To(gwv1.PathMatchExact), Value: ptr.To(p)}}
}

func route(name string, age int, hosts []string, rules ...gwv1.HTTPRouteRule) *gwv1.HTTPRoute {
	r := &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1, CreationTimestamp: metav1.NewTime(t0.Add(time.Duration(age) * time.Minute))},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
			Rules:           rules,
		},
	}
	for _, h := range hosts {
		r.Spec.Hostnames = append(r.Spec.Hostnames, gwv1.Hostname(h))
	}
	return r
}

func rule(backendName string, matches ...gwv1.HTTPRouteMatch) gwv1.HTTPRouteRule {
	return gwv1.HTTPRouteRule{Matches: matches, BackendRefs: []gwv1.HTTPBackendRef{backendRef(backendName, 80)}}
}

func rulesOf(c *cloudflare.TunnelConfig) []string {
	var out []string
	for _, r := range c.Ingress {
		s := strings.Join(strings.Fields(r.Hostname+" "+r.Path+" "+r.Service), " ")
		if r.OriginRequest != nil {
			b, _ := json.Marshal(r.OriginRequest)
			s += " " + string(b)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func parentCond(t *testing.T, res *Result, name string, typ string) *metav1.Condition {
	t.Helper()
	for _, r := range res.Routes {
		if r.Ref.Name == name {
			if len(r.Parents) == 0 {
				t.Fatalf("route %s has no parent results", name)
			}
			return meta.FindStatusCondition(r.Parents[0].Conditions, typ)
		}
	}
	t.Fatalf("route %s not in result", name)
	return nil
}

func TestListeners(t *testing.T) {
	gw := gateway(
		httpListener(),
		gwv1.Listener{Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443},
		gwv1.Listener{Name: "http8080", Protocol: gwv1.HTTPProtocolType, Port: 8080},
		gwv1.Listener{Name: "tls", Protocol: gwv1.TLSProtocolType, Port: 443},
		gwv1.Listener{Name: "tcp", Protocol: gwv1.TCPProtocolType, Port: 5432},
		gwv1.Listener{Name: "tcp-host", Protocol: gwv1.TCPProtocolType, Port: 5432, Hostname: ptr.To(gwv1.Hostname("db.example.com"))},
		gwv1.Listener{Name: "kinds", Protocol: gwv1.HTTPProtocolType, Port: 80, AllowedRoutes: &gwv1.AllowedRoutes{Kinds: []gwv1.RouteGroupKind{{Kind: "TLSRoute"}}}},
	)
	res := Translate(&Input{Gateway: gw, TCPRoute: true, TCPMode: true, GRPCRoute: true})
	want := map[string]string{
		"http": "Accepted", "https": "Accepted", "http8080": "PortUnavailable", "tls": "UnsupportedProtocol",
		"tcp": "UnsupportedValue", "tcp-host": "Accepted", "kinds": "Accepted",
	}
	for _, l := range res.Listeners {
		c := meta.FindStatusCondition(l.Conditions, "Accepted")
		if c.Reason != want[string(l.Name)] {
			t.Errorf("listener %s: %s (%s)", l.Name, c.Reason, c.Message)
		}
	}
	if k := res.Listeners[0].SupportedKinds; len(k) != 2 || k[0].Kind != KindHTTPRoute || k[1].Kind != KindGRPCRoute {
		t.Errorf("http kinds: %v", k)
	}
	if rr := meta.FindStatusCondition(res.Listeners[6].Conditions, "ResolvedRefs"); rr.Reason != "InvalidRouteKinds" || res.Listeners[6].Valid {
		t.Errorf("kinds listener: %+v valid=%v", rr, res.Listeners[6].Valid)
	}
	if !res.Accepted || res.AcceptedReason != "ListenersNotValid" {
		t.Errorf("gateway: %v %s", res.Accepted, res.AcceptedReason)
	}

	// TCP listeners without TCP mode.
	res = Translate(&Input{Gateway: gateway(gw.Spec.Listeners[5]), TCPRoute: true})
	if res.Accepted || res.AcceptedReason != "ListenersNotValid" {
		t.Errorf("tcp without TCP mode accepted: %+v", res.Listeners)
	}
}

func TestPathsAndPrecedence(t *testing.T) {
	gw := gateway(httpListener())
	older := route("older", 0, []string{"app.example.com"},
		rule("root", prefix("/")),
		rule("api", prefix("/api/")),
	)
	newer := route("newer", 5, []string{"app.example.com", "*.example.com"},
		rule("exact", exact("/healthz")),
		rule("v1", prefix("/api/v1")),
		rule("dup", prefix("/api")), // conflicts with older's /api
	)
	wild := route("deep", 7, []string{"*.a.example.com"}, rule("deep"))
	res := Translate(&Input{Gateway: gw, HTTPRoutes: []*gwv1.HTTPRoute{newer, older, wild}})
	want := []string{
		"app.example.com ^/healthz$ http://exact.default.svc.cluster.local:80",
		"app.example.com ^/api/v1(?:/.*)?$ http://v1.default.svc.cluster.local:80",
		"app.example.com ^/api(?:/.*)?$ http://api.default.svc.cluster.local:80",
		"app.example.com http://root.default.svc.cluster.local:80",
		"*.a.example.com http://deep.default.svc.cluster.local:80",
		"*.example.com ^/healthz$ http://exact.default.svc.cluster.local:80",
		"*.example.com ^/api/v1(?:/.*)?$ http://v1.default.svc.cluster.local:80",
		"*.example.com ^/api(?:/.*)?$ http://dup.default.svc.cluster.local:80",
		"http_status:404",
	}
	got := rulesOf(res.Config)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c := parentCond(t, res, "newer", "PartiallyInvalid"); c == nil || !strings.Contains(c.Message, "already published by HTTPRoute default/older") {
		t.Errorf("newer: %+v", c)
	}
	if res.Listeners[0].AttachedRoutes != 3 {
		t.Errorf("attached: %d", res.Listeners[0].AttachedRoutes)
	}
	if *res.Config.OriginRequest.KeepAliveConnections != -1 {
		t.Error("connection reuse not disabled")
	}
	if len(res.Hostnames) != 3 || res.Hostnames[0].Name != "*.a.example.com" {
		t.Errorf("hostnames: %+v", res.Hostnames)
	}
}

func TestRegexTranslationMatches(t *testing.T) {
	for _, c := range []struct {
		re      string
		match   []string
		noMatch []string
	}{
		{ExactPathRegex("/a.b"), []string{"/a.b"}, []string{"/aXb", "/a.b/", "/a.bc"}},
		{PrefixPathRegex("/api/"), []string{"/api", "/api/", "/api/x/y"}, []string{"/apix", "/ap"}},
	} {
		re := regexp.MustCompile(c.re)
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%s should match %s", c.re, p)
			}
		}
		for _, p := range c.noMatch {
			if re.MatchString(p) {
				t.Errorf("%s should not match %s", c.re, p)
			}
		}
	}
	if PrefixPathRegex("/") != "" {
		t.Error("prefix / must be a rule without a path")
	}
}

func TestHostnames(t *testing.T) {
	gw := gateway(httpListener(), gwv1.Listener{Name: "named", Protocol: gwv1.HTTPProtocolType, Port: 80, Hostname: ptr.To(gwv1.Hostname("*.example.com"))})
	hostless := route("hostless", 0, nil, rule("a"))
	hostless.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("http"))
	inherit := route("inherit", 1, nil, rule("b"))
	inherit.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("named"))
	miss := route("miss", 2, []string{"other.org"}, rule("c"))
	miss.Spec.ParentRefs[0].SectionName = ptr.To(gwv1.SectionName("named"))
	narrow := route("narrow", 3, []string{"x.example.com"}, rule("d"))
	res := Translate(&Input{Gateway: gw, HTTPRoutes: []*gwv1.HTTPRoute{hostless, inherit, miss, narrow}})
	if c := parentCond(t, res, "hostless", "Accepted"); c.Status != "False" || c.Reason != "UnsupportedValue" {
		t.Errorf("hostless: %+v", c)
	}
	if c := parentCond(t, res, "miss", "Accepted"); c.Reason != "NoMatchingListenerHostname" {
		t.Errorf("miss: %+v", c)
	}
	got := strings.Join(rulesOf(res.Config), "\n")
	if !strings.Contains(got, "*.example.com http://b.") || !strings.Contains(got, "x.example.com http://d.") {
		t.Errorf("rules: %s", got)
	}
}

func TestUnsupportedAndBackends(t *testing.T) {
	gw := gateway(httpListener())
	r := route("r", 0, []string{"app.example.com"},
		gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{{Headers: []gwv1.HTTPHeaderMatch{{Name: "x", Value: "y"}}}}, BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80)}},
		gwv1.HTTPRouteRule{
			Matches:     []gwv1.HTTPRouteMatch{prefix("/rewrite")},
			Filters:     []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterURLRewrite, URLRewrite: &gwv1.HTTPURLRewriteFilter{Hostname: ptr.To(gwv1.PreciseHostname("internal.example"))}}},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80)},
		},
		gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{prefix("/redirect")},
			Filters:     []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gwv1.HTTPRequestRedirectFilter{}}},
			BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80)}},
		gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{prefix("/split")}, BackendRefs: []gwv1.HTTPBackendRef{
			backendRef("web", 80), {BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "tls", Port: ptr.To(gwv1.PortNumber(443))}, Weight: ptr.To(int32(5))}},
		}},
		rule("missing", prefix("/missing")),
		gwv1.HTTPRouteRule{Matches: []gwv1.HTTPRouteMatch{prefix("/other-ns")}, BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
			Name: "web", Namespace: ptr.To(gwv1.Namespace("other")), Port: ptr.To(gwv1.PortNumber(80))}}}}},
	)
	in := &Input{Gateway: gw, HTTPRoutes: []*gwv1.HTTPRoute{r},
		GetService: services(
			svc("default", "web", corev1.ServicePort{Port: 80}),
			svc("default", "tls", corev1.ServicePort{Port: 443, AppProtocol: ptr.To("https")}),
			svc("other", "web", corev1.ServicePort{Port: 80, AppProtocol: ptr.To("kubernetes.io/h2c")}),
		)}
	res := Translate(in)
	got := strings.Join(rulesOf(res.Config), "\n")
	for _, w := range []string{
		`app.example.com ^/rewrite(?:/.*)?$ http://web.default.svc.cluster.local:80 {"httpHostHeader":"internal.example"}`,
		`app.example.com ^/split(?:/.*)?$ https://tls.default.svc.cluster.local:443 {"noTLSVerify":true}`,
		`app.example.com ^/missing(?:/.*)?$ http_status:503`,
		`app.example.com ^/other-ns(?:/.*)?$ http_status:503`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	if strings.Contains(got, "redirect") || strings.Count(got, "\n") != 4 {
		t.Errorf("unexpected rules:\n%s", got)
	}
	acc := parentCond(t, res, "r", "Accepted")
	if acc.Status != "True" || !strings.Contains(acc.Message, "traffic splitting") {
		t.Errorf("accepted: %+v", acc)
	}
	pi := parentCond(t, res, "r", "PartiallyInvalid")
	if pi == nil || !strings.Contains(pi.Message, "header") || !strings.Contains(pi.Message, "RequestRedirect") {
		t.Errorf("partially invalid: %+v", pi)
	}
	rr := parentCond(t, res, "r", "ResolvedRefs")
	if rr.Status != "False" || rr.Reason != "BackendNotFound" || !strings.Contains(rr.Message, "ReferenceGrant") {
		t.Errorf("resolved: %+v", rr)
	}

	// A ReferenceGrant lets the other namespace through; h2c becomes HTTP/1.1.
	in.ReferenceGrants = []*gwv1.ReferenceGrant{{ObjectMeta: metav1.ObjectMeta{Namespace: "other"}, Spec: gwv1.ReferenceGrantSpec{
		From: []gwv1.ReferenceGrantFrom{{Group: gwv1.GroupName, Kind: KindHTTPRoute, Namespace: "default"}},
		To:   []gwv1.ReferenceGrantTo{{Group: "", Kind: KindService}},
	}}}
	got = strings.Join(rulesOf(Translate(in).Config), "\n")
	if !strings.Contains(got, `app.example.com ^/other-ns(?:/.*)?$ http://web.other.svc.cluster.local:80`) {
		t.Errorf("granted:\n%s", got)
	}

	// Nothing usable.
	only := route("only", 0, []string{"x.example.com"}, gwv1.HTTPRouteRule{Timeouts: &gwv1.HTTPRouteTimeouts{}, BackendRefs: []gwv1.HTTPBackendRef{backendRef("web", 80)}})
	res = Translate(&Input{Gateway: gw, HTTPRoutes: []*gwv1.HTTPRoute{only}})
	if c := parentCond(t, res, "only", "Accepted"); c.Status != "False" || c.Reason != "UnsupportedValue" {
		t.Errorf("only: %+v", c)
	}
}

func TestNamespacesAndPolicy(t *testing.T) {
	gw := gateway(gwv1.Listener{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80,
		AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromSelector),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}}}})
	a := route("a", 0, []string{"a.example.com", "evil.other.org"}, rule("web"))
	a.Namespace = "team-a"
	a.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("default"))
	b := route("b", 1, []string{"b.example.com"}, rule("web"))
	b.Namespace = "team-b"
	b.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("default"))
	derived := route("derived", 2, []string{"derived.apps.example.net"}, rule("web"))
	derived.Namespace = "team-a"
	derived.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("default"))
	derived.Annotations = map[string]string{config.AnnotationDefaultHostname: "derived.apps.example.net"}
	claimed := route("claimed", 3, []string{"taken.example.com"}, rule("web"))
	claimed.Namespace = "team-a"
	claimed.Spec.ParentRefs[0].Namespace = ptr.To(gwv1.Namespace("default"))
	res := Translate(&Input{
		Gateway:         gw,
		HTTPRoutes:      []*gwv1.HTTPRoute{a, b, derived, claimed},
		NamespaceLabels: map[string]map[string]string{"team-a": {"team": "a"}, "team-b": {"team": "b"}},
		AllowedHostnames: func(ns string) []string {
			if ns == "team-a" {
				return []string{"*.example.com"}
			}
			return nil
		},
		Claimed: map[string]Claim{"taken.example.com": {Gateway: "other/gw", Routes: []RouteRef{{KindHTTPRoute, "other", "x"}}}},
	})
	if c := parentCond(t, res, "b", "Accepted"); c.Reason != "NotAllowedByListeners" {
		t.Errorf("b: %+v", c)
	}
	if c := parentCond(t, res, "a", "PartiallyInvalid"); c == nil || !strings.Contains(c.Message, "evil.other.org is not allowed") {
		t.Errorf("a: %+v", c)
	}
	if c := parentCond(t, res, "derived", "Accepted"); c.Status != "True" {
		t.Errorf("derived hostnames are exempt: %+v", c)
	}
	if c := parentCond(t, res, "claimed", "Accepted"); c.Reason != ReasonConflicted {
		t.Errorf("claimed: %+v", c)
	}
	got := strings.Join(rulesOf(res.Config), "\n")
	if strings.Contains(got, "evil") || strings.Contains(got, "taken") || !strings.Contains(got, "derived.apps.example.net") {
		t.Errorf("rules:\n%s", got)
	}
}

func TestGRPC(t *testing.T) {
	gw := gateway(httpListener())
	g := &gwv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default", CreationTimestamp: metav1.NewTime(t0)},
		Spec: gwv1.GRPCRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
			Hostnames:       []gwv1.Hostname{"grpc.example.com"},
			Rules: []gwv1.GRPCRouteRule{
				{Matches: []gwv1.GRPCRouteMatch{{Method: &gwv1.GRPCMethodMatch{Service: ptr.To("pkg.Echo"), Method: ptr.To("Say")}}},
					BackendRefs: []gwv1.GRPCBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "tls", Port: ptr.To(gwv1.PortNumber(443))}}}}},
				{Matches: []gwv1.GRPCRouteMatch{{Method: &gwv1.GRPCMethodMatch{Service: ptr.To("pkg.Plain")}}},
					BackendRefs: []gwv1.GRPCBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: "plain", Port: ptr.To(gwv1.PortNumber(80))}}}}},
			},
		},
	}
	res := Translate(&Input{Gateway: gw, GRPCRoute: true, GRPCRoutes: []*gwv1.GRPCRoute{g}, GetService: services(
		svc("default", "tls", corev1.ServicePort{Port: 443, AppProtocol: ptr.To("https")}),
		svc("default", "plain", corev1.ServicePort{Port: 80}),
	)})
	got := strings.Join(rulesOf(res.Config), "\n")
	want := "grpc.example.com ^/pkg\\.Echo/Say$ https://tls.default.svc.cluster.local:443 {\"http2Origin\":true,\"noTLSVerify\":true}\n" +
		"grpc.example.com ^/pkg\\.Plain/[^/]+$ http_status:503\nhttp_status:404"
	if got != want {
		t.Errorf("rules:\n%s\nwant:\n%s", got, want)
	}
	if c := parentCond(t, res, "g", "ResolvedRefs"); c.Reason != "UnsupportedProtocol" {
		t.Errorf("resolved: %+v", c)
	}
	if !res.GRPC {
		t.Error("GRPC not reported")
	}
	for _, c := range []struct {
		m    *gwv1.GRPCMethodMatch
		path string
		ok   bool
	}{
		{&gwv1.GRPCMethodMatch{Method: ptr.To("Say")}, "/a.B/Say", true},
		{&gwv1.GRPCMethodMatch{Method: ptr.To("Say")}, "/a.B/Sayx", false},
		{&gwv1.GRPCMethodMatch{Type: ptr.To(gwv1.GRPCMethodMatchRegularExpression), Service: ptr.To("pkg\\..*")}, "/pkg.X/Any", true},
		{&gwv1.GRPCMethodMatch{Type: ptr.To(gwv1.GRPCMethodMatchRegularExpression), Service: ptr.To("pkg\\..*")}, "/other.X/Any", false},
	} {
		re, _, _, err := grpcPath(c.m)
		if err != nil || regexp.MustCompile(re).MatchString(c.path) != c.ok {
			t.Errorf("%s on %s: %v", re, c.path, err)
		}
	}
}

func TestTCP(t *testing.T) {
	gw := gateway(httpListener(),
		gwv1.Listener{Name: "db", Protocol: gwv1.TCPProtocolType, Port: 5432, Hostname: ptr.To(gwv1.Hostname("db.example.com"))})
	tr := &gwv1.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", CreationTimestamp: metav1.NewTime(t0)},
		Spec: gwv1.TCPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw", SectionName: ptr.To(gwv1.SectionName("db"))}}},
			Rules:           []gwv1.TCPRouteRule{{BackendRefs: []gwv1.BackendRef{{BackendObjectReference: gwv1.BackendObjectReference{Name: "pg", Port: ptr.To(gwv1.PortNumber(5432))}}}}},
		},
	}
	clash := route("clash", 1, []string{"db.example.com"}, rule("web"))
	web := route("web", 2, []string{"app.example.com"}, rule("web"))
	res := Translate(&Input{Gateway: gw, TCPRoute: true, TCPMode: true, TCPRoutes: []*gwv1.TCPRoute{tr}, HTTPRoutes: []*gwv1.HTTPRoute{clash, web}})
	got := rulesOf(res.Config)
	want := []string{"db.example.com tcp://pg.default.svc.cluster.local:5432", "app.example.com http://web.default.svc.cluster.local:80", "http_status:404"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules: %v", got)
	}
	if c := parentCond(t, res, "clash", "Accepted"); c.Reason != ReasonConflicted {
		t.Errorf("clash: %+v", c)
	}
	if !res.Hostnames[1].TCP || res.Hostnames[1].Name != "db.example.com" {
		t.Errorf("hostnames: %+v", res.Hostnames)
	}
	if res.Listeners[1].AttachedRoutes != 1 {
		t.Errorf("attached: %+v", res.Listeners[1])
	}
}

func TestDNSParent(t *testing.T) {
	gw := gateway(httpListener())
	r := route("r", 0, []string{"app.example.com"}, rule("web"))
	r.Annotations = map[string]string{config.AnnotationDNSParent: "default/new"}
	res := Translate(&Input{Gateway: gw, HTTPRoutes: []*gwv1.HTTPRoute{r}})
	if res.Hostnames[0].DNSParent != "default/new" || res.Hostnames[0].Routes[0] != (RouteRef{KindHTTPRoute, "default", "r"}) {
		t.Errorf("%+v", res.Hostnames)
	}
}

func TestHostnameAllowed(t *testing.T) {
	for _, c := range []struct {
		host string
		pats []string
		ok   bool
	}{
		{"a.example.com", []string{"a.example.com"}, true},
		{"b.example.com", []string{"a.example.com"}, false},
		{"x.y.example.com", []string{"*.example.com"}, true},
		{"*.y.example.com", []string{"*.example.com"}, true},
		{"example.com", []string{"*.example.com"}, false},
		{"evilexample.com", []string{"*.example.com"}, false},
	} {
		if HostnameAllowed(c.host, c.pats) != c.ok {
			t.Errorf("%s %v", c.host, c.pats)
		}
	}
}
