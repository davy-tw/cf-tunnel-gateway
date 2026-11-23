// Package translate turns a Gateway and the routes attached to it into a
// Cloudflare Tunnel remote configuration, plus the Gateway, listener and
// route status that goes with it. It is pure: all cluster state comes in
// through Input.
package translate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// Kinds.
const (
	KindHTTPRoute = "HTTPRoute"
	KindGRPCRoute = "GRPCRoute"
	KindTCPRoute  = "TCPRoute"
	KindGateway   = "Gateway"
	KindService   = "Service"
	// KindListenerSet adds listeners to a Gateway; routes attach to them
	// through a parentRef of this kind.
	KindListenerSet = "ListenerSet"
)

// Implementation-specific route reasons.
const (
	// ReasonConflicted: the hostname and path, or the hostname, are
	// already published by an older route or Gateway.
	ReasonConflicted = "Conflicted"
	// ReasonHostnameNotAllowed: the namespace's allowed-hostnames policy
	// does not allow the hostnames. It is also the Accepted reason of a
	// ListenerSet listener whose hostname the ListenerSet's namespace may
	// not publish.
	ReasonHostnameNotAllowed = "HostnameNotAllowed"
)

// ConditionDNSRecordsReady is an implementation-specific route parent
// condition: the DNS records of the route's hostnames point at this
// Gateway's tunnel.
const ConditionDNSRecordsReady = "DNSRecordsReady"

// Claim is a hostname published by another, older Gateway.
type Claim struct {
	Gateway string     // namespace/name
	Routes  []RouteRef // the routes that publish it there
}

// Input is everything the translation needs.
type Input struct {
	Gateway *gwv1.Gateway

	HTTPRoutes []*gwv1.HTTPRoute
	GRPCRoutes []*gwv1.GRPCRoute
	TCPRoutes  []*gwv1.TCPRoute

	ReferenceGrants []*gwv1.ReferenceGrant

	// NamespaceLabels maps namespace name to its labels, for listener
	// allowedRoutes selectors.
	NamespaceLabels map[string]map[string]string
	// GetService returns a backend Service, or nil if it does not exist.
	// When GetService itself is nil, every backend is assumed to exist.
	GetService func(namespace, name string) *corev1.Service
	// AllowedHostnames returns a namespace's hostname patterns; nil means
	// unrestricted, an empty non-nil slice allows nothing.
	AllowedHostnames func(namespace string) []string

	ClusterDomain string
	// GRPCRoute and TCPRoute are served by the cluster; TCPMode is the
	// controller's TCP mode switch.
	GRPCRoute, TCPRoute, TCPMode bool
	// Claimed maps hostnames to the other Gateways that keep them.
	Claimed map[string]Claim
	// Generated returns, for a route the controller generated, its
	// derived hostnames (exempt from allowed-hostnames) and its DNS
	// parent. The annotations that carry them are ignored on every other
	// route, since route authors can write them. Nil trusts no route.
	Generated func(m metav1.ObjectMeta) (derived map[string]bool, dnsParent string)

	// ListenerSets are the ListenerSets whose parentRef names the Gateway
	// (others are ignored).
	ListenerSets []*gwv1.ListenerSet
	// HostOwners is the owner of each hostname within the Gateway, as
	// recorded by earlier reconciles (Result.HostOwners): OwnerGateway or
	// ListenerSetOwner. The first owner to use a hostname keeps it.
	HostOwners map[string]string
}

// RouteRef identifies a route.
type RouteRef struct {
	Kind      string
	Namespace string
	Name      string
}

func (r RouteRef) String() string { return r.Kind + " " + r.Namespace + "/" + r.Name }

// ParseRouteRef parses RouteRef.String.
func ParseRouteRef(s string) (RouteRef, bool) {
	kind, rest, ok := strings.Cut(s, " ")
	if !ok {
		return RouteRef{}, false
	}
	ns, name, ok := strings.Cut(rest, "/")
	return RouteRef{Kind: kind, Namespace: ns, Name: name}, ok
}

// ParentResult is the status of a route for one parentRef that points at
// this Gateway.
type ParentResult struct {
	ParentRef  gwv1.ParentReference
	Conditions []metav1.Condition
	// Hostnames are the hostnames the route publishes through this parent.
	Hostnames []string
}

// RouteResult is the status of one route with respect to this Gateway.
type RouteResult struct {
	Ref        RouteRef
	Generation int64
	Parents    []ParentResult
}

// ListenerResult is the status of one listener, without Programmed.
type ListenerResult struct {
	Name           gwv1.SectionName
	SupportedKinds []gwv1.RouteGroupKind
	AttachedRoutes int32
	Conditions     []metav1.Condition
	Valid          bool
	// InvalidReason is the Programmed reason of a listener that is not
	// valid; empty means Invalid.
	InvalidReason string
}

// Hostname is a hostname the tunnel publishes.
type Hostname struct {
	Name string
	TCP  bool
	// Routes publishing it, oldest first.
	Routes []RouteRef
	// DNSParent is the Gateway ("namespace/name") that a switching route
	// names as the owner of the hostname's DNS records, or "".
	DNSParent string
	// DNSFrom lists the Gateways ("namespace/name") that route is attached
	// to: DNS records may only move to DNSParent from one of them.
	DNSFrom []string
}

// Result is the output of Translate.
type Result struct {
	Config    *cloudflare.TunnelConfig
	Listeners []ListenerResult
	Routes    []RouteResult
	Hostnames []Hostname
	// GRPC is set when a GRPCRoute is attached.
	GRPC bool
	// Accepted, AcceptedReason and AcceptedMessage describe the Gateway's
	// Accepted condition as far as listeners go.
	Accepted        bool
	AcceptedReason  string
	AcceptedMessage string

	// ListenerSets is in listener precedence order (oldest first).
	ListenerSets []ListenerSetResult
	// AttachedListenerSets counts the accepted ListenerSets.
	AttachedListenerSets int32
	// HostOwners is the owner of each hostname the Gateway publishes or
	// a ListenerSet listener declares, to record for the next reconcile.
	HostOwners map[string]string
}

// Translate builds the tunnel configuration and statuses.
func Translate(in *Input) *Result {
	t := &translator{in: in, byKey: map[ruleKey]*entry{}, hostKind: map[string]hostOwner{},
		sets: map[types.NamespacedName]*setState{}, owner: map[string]int{}, declared: map[string]int{}, gatewayWildcards: map[string]bool{},
		parentResults: map[RouteRef]map[int]ParentResult{}}
	return t.run()
}

type translator struct {
	in        *Input
	listeners []*listenerState
	entries   []*entry
	byKey     map[ruleKey]*entry
	hostKind  map[string]hostOwner
	routes    []RouteResult
	grpc      bool

	// sets holds every ListenerSet naming the Gateway; setOrder is their
	// precedence order.
	sets     map[types.NamespacedName]*setState
	setOrder []*setState
	// owner maps a hostname to the rank of the owner that holds it within
	// the Gateway: 0 for the Gateway, then the allowed ListenerSets in
	// precedence order. A hostname is served on every port and path at
	// once, so one owner holds all of it.
	owner map[string]int
	// declared maps the hostnames of accepted ListenerSet listeners to
	// their owner's rank.
	declared map[string]int
	// gatewayWildcards are the wildcard hostnames the Gateway publishes
	// through its routes; they cover the names below them against
	// ListenerSets.
	gatewayWildcards map[string]bool
	// earlier holds the accepted listeners that later ListenerSet
	// listeners must not conflict with.
	earlier []*listenerState
	// parentResults collects route results per parentRef index while
	// routes are processed owner by owner.
	parentResults map[RouteRef]map[int]ParentResult
}

type hostOwner struct {
	tcp   bool
	owner RouteRef
}

type ruleKey struct{ host, path string }

type pathKind int

const (
	pathExact pathKind = iota
	pathRegex
	pathPrefix
	pathAll
)

// entry is one tunnel ingress rule before ordering.
type entry struct {
	rule     cloudflare.IngressRule
	tcp      bool
	kind     pathKind
	sortLen  int
	owner    RouteRef
	created  metav1.Time
	ruleIdx  int
	matchIdx int
	dnsPar   string
	dnsFrom  []string
}

type listenerState struct {
	l     gwv1.Listener
	res   ListenerResult
	kinds map[string]bool
	// set is the ListenerSet that defines the listener; nil for the
	// Gateway's own listeners.
	set *setState
	// ns is the namespace of the listener's owner, which allowedRoutes
	// "Same" refers to.
	ns string
	// accepted is false for listeners that can never carry traffic
	// (unsupported protocol or port).
	accepted bool
}

// rank is the precedence of the listener's owner: 0 for the Gateway.
func (ls *listenerState) rank() int {
	if ls.set == nil {
		return 0
	}
	return ls.set.rank
}

func (t *translator) gen() int64 { return t.in.Gateway.Generation }

func (t *translator) run() *Result {
	t.buildListeners()
	// Owners go in precedence order: the Gateway's listeners and routes,
	// then each allowed ListenerSet's listeners and routes. The hostnames
	// an earlier owner uses are settled before a later owner's listeners
	// and routes are checked against them; hostnames recorded by earlier
	// reconciles stay with their owner whatever the order.
	t.prepareListenerSets()
	t.seedOwners()
	routed := t.collectRoutes()
	t.processRank(routed, 0)
	for _, ss := range t.setOrder {
		if ss.allowed {
			t.buildSetListeners(ss)
			t.processRank(routed, ss.rank)
		}
	}
	t.assembleRoutes(routed)

	res := &Result{Routes: t.routes, GRPC: t.grpc}
	valid, own := 0, 0
	for _, ls := range t.listeners {
		if ls.set != nil {
			continue
		}
		res.Listeners = append(res.Listeners, ls.res)
		own++
		if ls.res.Valid {
			valid++
		}
	}
	res.Config, res.Hostnames = t.render()
	switch {
	case own == 0:
		// Every listener comes from ListenerSets.
		res.Accepted = true
		res.AcceptedReason = string(gwv1.GatewayReasonAccepted)
		res.AcceptedMessage = "Gateway accepted"
	case valid == 0:
		res.AcceptedReason = string(gwv1.GatewayReasonListenersNotValid)
		res.AcceptedMessage = "no listener is valid"
	case valid < own:
		res.Accepted = true
		res.AcceptedReason = string(gwv1.GatewayReasonListenersNotValid)
		res.AcceptedMessage = fmt.Sprintf("%d of %d listeners are not valid", own-valid, own)
	default:
		res.Accepted = true
		res.AcceptedReason = string(gwv1.GatewayReasonAccepted)
		res.AcceptedMessage = "Gateway accepted"
	}
	res.ListenerSets, res.AttachedListenerSets = t.listenerSetResults(res.Accepted)
	res.HostOwners = t.hostOwners(res.Hostnames)
	return res
}

// ---- listeners ----

func (t *translator) compatibleKinds(p gwv1.ProtocolType) []string {
	switch p {
	case gwv1.HTTPProtocolType, gwv1.HTTPSProtocolType:
		out := []string{KindHTTPRoute}
		if t.in.GRPCRoute {
			out = append(out, KindGRPCRoute)
		}
		return out
	case gwv1.TCPProtocolType:
		if t.in.TCPRoute && t.in.TCPMode {
			return []string{KindTCPRoute}
		}
	}
	return nil
}

func (t *translator) buildListeners() {
	for _, l := range t.in.Gateway.Spec.Listeners {
		ls := t.newListener(l, t.in.Gateway.Namespace, nil, t.gen())
		t.listeners = append(t.listeners, ls)
		// The Gateway's listener hostnames are the Gateway's: its owner
		// grants ListenerSets their place.
		if h := listenerHostname(l.Hostname); ls.accepted && h != "" {
			t.owner[h] = 0
		}
	}
}

// newListener checks a listener of the Gateway (set nil) or of a
// ListenerSet, whose namespace is ns and generation gen.
func (t *translator) newListener(l gwv1.Listener, ns string, set *setState, gen int64) *listenerState {
	ls := &listenerState{l: l, kinds: map[string]bool{}, set: set, ns: ns}
	ls.res.Name = l.Name
	var conds []metav1.Condition
	accepted := true
	reject := func(reason gwv1.ListenerConditionReason, msg string) {
		accepted = false
		ls.res.InvalidReason = string(reason)
		conds = append(conds, cond(gwv1.ListenerConditionAccepted, false, reason, msg, gen))
	}
	switch l.Protocol {
	case gwv1.HTTPProtocolType:
		if l.Port != 80 {
			reject(gwv1.ListenerReasonPortUnavailable, fmt.Sprintf("HTTP listeners must use port 80: Cloudflare serves a published hostname on all its ports at once, so port %d cannot restrict anything", l.Port))
		} else {
			conds = append(conds, cond(gwv1.ListenerConditionAccepted, true, gwv1.ListenerReasonAccepted, "Listener accepted", gen))
		}
	case gwv1.HTTPSProtocolType:
		switch {
		case l.Port != 443:
			reject(gwv1.ListenerReasonPortUnavailable, fmt.Sprintf("HTTPS listeners must use port 443: Cloudflare serves a published hostname on all its ports at once, so port %d cannot restrict anything", l.Port))
		case l.TLS != nil && l.TLS.Mode != nil && *l.TLS.Mode == gwv1.TLSModePassthrough:
			reject(gwv1.ListenerReasonUnsupportedValue, "TLS passthrough is not supported: Cloudflare terminates TLS at its edge")
		default:
			conds = append(conds, cond(gwv1.ListenerConditionAccepted, true, gwv1.ListenerReasonAccepted,
				"Listener accepted; TLS is terminated at Cloudflare's edge with edge certificates, and any referenced certificate is ignored", gen))
		}
	case gwv1.TCPProtocolType:
		switch {
		case !t.in.TCPMode:
			reject(gwv1.ListenerReasonUnsupportedProtocol, "TCP listeners need the controller's TCP mode")
		case !t.in.TCPRoute:
			reject(gwv1.ListenerReasonUnsupportedProtocol, "TCP listeners need the TCPRoute CRD")
		default:
			conds = append(conds, cond(gwv1.ListenerConditionAccepted, true, gwv1.ListenerReasonAccepted,
				"Listener accepted; the port is not exposed: each TCPRoute names its hostname with annotation "+config.AnnotationHostname+
					", and clients reach it with cloudflared access tcp", gen))
		}
	default:
		reject(gwv1.ListenerReasonUnsupportedProtocol, fmt.Sprintf("protocol %q is not supported", l.Protocol))
	}

	compat := t.compatibleKinds(l.Protocol)
	allowed := map[string]bool{}
	for _, k := range compat {
		allowed[k] = true
	}
	var invalidKinds []string
	if l.AllowedRoutes != nil && len(l.AllowedRoutes.Kinds) > 0 {
		for _, rk := range l.AllowedRoutes.Kinds {
			group := gwv1.GroupName
			if rk.Group != nil {
				group = string(*rk.Group)
			}
			if group == gwv1.GroupName && allowed[string(rk.Kind)] {
				ls.kinds[string(rk.Kind)] = true
			} else {
				invalidKinds = append(invalidKinds, string(rk.Kind))
			}
		}
	} else {
		for k := range allowed {
			ls.kinds[k] = true
		}
	}
	for _, k := range compat {
		if ls.kinds[k] {
			g := gwv1.Group(gwv1.GroupName)
			ls.res.SupportedKinds = append(ls.res.SupportedKinds, gwv1.RouteGroupKind{Group: &g, Kind: gwv1.Kind(k)})
		}
	}
	resolved := len(invalidKinds) == 0
	if !resolved {
		conds = append(conds, cond(gwv1.ListenerConditionResolvedRefs, false, gwv1.ListenerReasonInvalidRouteKinds,
			fmt.Sprintf("unsupported route kinds for protocol %s: %s", l.Protocol, strings.Join(invalidKinds, ", ")), gen))
	} else {
		conds = append(conds, cond(gwv1.ListenerConditionResolvedRefs, true, gwv1.ListenerReasonResolvedRefs, "References resolved", gen))
	}
	conds = append(conds, cond(gwv1.ListenerConditionConflicted, false, gwv1.ListenerReasonNoConflicts, "No conflicts", gen))
	ls.res.Conditions = conds
	ls.accepted = accepted
	ls.res.Valid = accepted && (resolved || len(ls.kinds) > 0)
	return ls
}

// listenerHostname normalizes a listener hostname; "" when there is none.
func listenerHostname(h *gwv1.Hostname) string {
	if h == nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(string(*h)), ".")
}

// ---- routes ----

type routeItem struct {
	ref        RouteRef
	created    metav1.Time
	generation int64
	parents    []gwv1.ParentReference
	dnsParent  string
	derived    map[string]bool
	process    func(*routeCtx)
}

type routeCtx struct {
	t    *translator
	item *routeItem
	// rank is the listener owner whose parentRefs this pass handles.
	rank        int
	unsupported []string
	unresolved  []string
	resolvedWhy gwv1.RouteConditionReason
	notes       []string
	// nothingUsable is set when every rule is unsupported.
	nothingUsable bool
}

func (rc *routeCtx) unsupportedf(format string, args ...any) {
	rc.unsupported = append(rc.unsupported, fmt.Sprintf(format, args...))
}

func (rc *routeCtx) unresolvedf(reason gwv1.RouteConditionReason, format string, args ...any) {
	if rc.resolvedWhy == "" {
		rc.resolvedWhy = reason
	}
	rc.unresolved = append(rc.unresolved, fmt.Sprintf(format, args...))
}

func (t *translator) routeMeta(m metav1.ObjectMeta, kind string, parents []gwv1.ParentReference, process func(*routeCtx)) *routeItem {
	it := &routeItem{ref: RouteRef{kind, m.Namespace, m.Name}, created: m.CreationTimestamp, generation: m.Generation,
		parents: parents, process: process}
	if t.in.Generated != nil {
		it.derived, it.dnsParent = t.in.Generated(m)
	}
	return it
}

// AnnotatedDerived parses a route's default-hostname annotation.
func AnnotatedDerived(m metav1.ObjectMeta) map[string]bool {
	v := m.Annotations[config.AnnotationDefaultHostname]
	if v == "" {
		return nil
	}
	out := map[string]bool{}
	for _, h := range strings.Split(v, ",") {
		out[strings.ToLower(strings.TrimSpace(h))] = true
	}
	return out
}

// collectRoutes returns the routes with a parentRef naming the Gateway or
// one of its ListenerSets, oldest first.
func (t *translator) collectRoutes() []*routeItem {
	var items []*routeItem
	for _, r := range t.in.HTTPRoutes {
		items = append(items, t.routeMeta(r.ObjectMeta, KindHTTPRoute, r.Spec.ParentRefs, func(rc *routeCtx) { rc.httpRoute(r) }))
	}
	for _, r := range t.in.GRPCRoutes {
		items = append(items, t.routeMeta(r.ObjectMeta, KindGRPCRoute, r.Spec.ParentRefs, func(rc *routeCtx) { rc.grpcRoute(r) }))
	}
	for _, r := range t.in.TCPRoutes {
		items = append(items, t.routeMeta(r.ObjectMeta, KindTCPRoute, r.Spec.ParentRefs, func(rc *routeCtx) { rc.tcpRoute(r) }))
	}
	sort.SliceStable(items, func(i, j int) bool { return olderItem(items[i], items[j]) })
	var routed []*routeItem
	for _, it := range items {
		if t.referencesGateway(it.parents, it.ref.Namespace) {
			routed = append(routed, it)
		}
	}
	return routed
}

// processRank places the routes of one listener owner, oldest first.
// Routes are placed owner by owner, the Gateway first, so that a
// higher-precedence owner takes new hostnames before later ListenerSets.
func (t *translator) processRank(routed []*routeItem, rank int) {
	for _, it := range routed {
		if t.hasParentOfRank(it, rank) {
			it.process(&routeCtx{t: t, item: it, rank: rank})
		}
	}
}

// assembleRoutes puts each route's parent results in parentRef order.
func (t *translator) assembleRoutes(routed []*routeItem) {
	for _, it := range routed {
		rr := RouteResult{Ref: it.ref, Generation: it.generation}
		for i := range it.parents {
			if pr, ok := t.parentResults[it.ref][i]; ok {
				rr.Parents = append(rr.Parents, pr)
			}
		}
		t.routes = append(t.routes, rr)
	}
}

// mine reports whether this pass handles the parentRef.
func (rc *routeCtx) mine(p gwv1.ParentReference) bool {
	return rc.t.parentRank(p, rc.item.ref.Namespace) == rc.rank
}

// record stores the result for the parentRef at index i.
func (rc *routeCtx) record(i int, pr ParentResult) {
	m := rc.t.parentResults[rc.item.ref]
	if m == nil {
		m = map[int]ParentResult{}
		rc.t.parentResults[rc.item.ref] = m
	}
	m[i] = pr
}

// parentRank returns the owner rank a parentRef names, or -1 when it names
// neither the Gateway nor one of its ListenerSets. ListenerSets the
// Gateway does not allow are handled with the Gateway (rank 0), which
// only reports them.
func (t *translator) parentRank(p gwv1.ParentReference, routeNS string) int {
	set, ok := t.parentOwner(p, routeNS)
	switch {
	case !ok:
		return -1
	case set == nil || !set.allowed:
		return 0
	}
	return set.rank
}

func (t *translator) hasParentOfRank(it *routeItem, rank int) bool {
	for _, p := range it.parents {
		if t.parentRank(p, it.ref.Namespace) == rank {
			return true
		}
	}
	return false
}

// parentOwner reports whether a parentRef names the Gateway (set is nil)
// or one of the ListenerSets that name the Gateway as their parent.
func (t *translator) parentOwner(p gwv1.ParentReference, routeNS string) (set *setState, ok bool) {
	if p.Group != nil && string(*p.Group) != gwv1.GroupName {
		return nil, false
	}
	ns := routeNS
	if p.Namespace != nil && *p.Namespace != "" {
		ns = string(*p.Namespace)
	}
	kind := KindGateway
	if p.Kind != nil {
		kind = string(*p.Kind)
	}
	switch kind {
	case KindGateway:
		return nil, ns == t.in.Gateway.Namespace && string(p.Name) == t.in.Gateway.Name
	case KindListenerSet:
		ss := t.sets[types.NamespacedName{Namespace: ns, Name: string(p.Name)}]
		return ss, ss != nil
	}
	return nil, false
}

func olderItem(a, b *routeItem) bool {
	if !a.created.Equal(&b.created) {
		return a.created.Before(&b.created)
	}
	if a.ref.Namespace != b.ref.Namespace {
		return a.ref.Namespace < b.ref.Namespace
	}
	if a.ref.Name != b.ref.Name {
		return a.ref.Name < b.ref.Name
	}
	return a.ref.Kind < b.ref.Kind
}

// ParentTargets reports whether p, from a route in routeNS, points at the
// Gateway ns/name.
func ParentTargets(p gwv1.ParentReference, routeNS, ns, name string) bool {
	if p.Group != nil && string(*p.Group) != gwv1.GroupName {
		return false
	}
	if p.Kind != nil && string(*p.Kind) != KindGateway {
		return false
	}
	pns := routeNS
	if p.Namespace != nil && *p.Namespace != "" {
		pns = string(*p.Namespace)
	}
	return pns == ns && string(p.Name) == name
}

func (t *translator) parentTargetsGateway(p gwv1.ParentReference, routeNS string) bool {
	_, ok := t.parentOwner(p, routeNS)
	return ok
}

func (t *translator) referencesGateway(parents []gwv1.ParentReference, routeNS string) bool {
	for _, p := range parents {
		if t.parentTargetsGateway(p, routeNS) {
			return true
		}
	}
	return false
}

type attachment struct {
	ls        *listenerState
	hostnames []string // nil: no hostname on either side
}

func (rc *routeCtx) attach(p gwv1.ParentReference, routeHostnames []gwv1.Hostname) ([]attachment, gwv1.RouteConditionReason, string) {
	t := rc.t
	set, _ := t.parentOwner(p, rc.item.ref.Namespace)
	if set != nil && !set.allowed {
		return nil, gwv1.RouteReasonNotAllowedByListeners, fmt.Sprintf("ListenerSet %s/%s is not allowed by Gateway %s/%s (spec.allowedListeners)",
			set.ls.Namespace, set.ls.Name, t.in.Gateway.Namespace, t.in.Gateway.Name)
	}
	var candidates []*listenerState
	for _, ls := range t.listeners {
		if ls.set != set {
			continue
		}
		if p.SectionName != nil && *p.SectionName != ls.l.Name {
			continue
		}
		if p.Port != nil && *p.Port != ls.l.Port {
			continue
		}
		candidates = append(candidates, ls)
	}
	if len(candidates) == 0 {
		return nil, gwv1.RouteReasonNoMatchingParent, "no listener matches the parentRef's sectionName or port"
	}
	var out []attachment
	msg := ""
	hostnameMiss := false
	for _, ls := range candidates {
		if !ls.res.Valid {
			msg = fmt.Sprintf("listener %q is not valid", ls.l.Name)
			continue
		}
		if !ls.kinds[rc.item.ref.Kind] {
			msg = fmt.Sprintf("listener %q does not allow %s", ls.l.Name, rc.item.ref.Kind)
			continue
		}
		if !t.namespaceAllowed(ls, rc.item.ref.Namespace) {
			msg = fmt.Sprintf("listener %q does not allow routes from namespace %q", ls.l.Name, rc.item.ref.Namespace)
			continue
		}
		hosts, ok := intersectHostnames(ls.l.Hostname, routeHostnames)
		if !ok {
			hostnameMiss = true
			continue
		}
		out = append(out, attachment{ls: ls, hostnames: hosts})
	}
	if len(out) == 0 {
		if hostnameMiss && msg == "" {
			return nil, gwv1.RouteReasonNoMatchingListenerHostname, "no listener hostname matches the route's hostnames"
		}
		if msg == "" {
			msg = "no listener allows this route"
		}
		return nil, gwv1.RouteReasonNotAllowedByListeners, msg
	}
	return out, "", ""
}

func (t *translator) namespaceAllowed(ls *listenerState, routeNS string) bool {
	from := gwv1.NamespacesFromSame
	var sel *metav1.LabelSelector
	if ar := ls.l.AllowedRoutes; ar != nil && ar.Namespaces != nil {
		if ar.Namespaces.From != nil {
			from = *ar.Namespaces.From
		}
		sel = ar.Namespaces.Selector
	}
	switch from {
	case gwv1.NamespacesFromAll:
		return true
	case gwv1.NamespacesFromSame:
		return routeNS == ls.ns
	case gwv1.NamespacesFromSelector:
		if sel == nil {
			return false
		}
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return false
		}
		return s.Matches(labels.Set(t.in.NamespaceLabels[routeNS]))
	}
	return false
}

// intersectHostnames applies the Gateway API listener/route hostname
// matching rules. It returns the effective hostnames (nil when neither side
// sets one) and whether they intersect.
func intersectHostnames(listener *gwv1.Hostname, route []gwv1.Hostname) ([]string, bool) {
	lh := ""
	if listener != nil {
		lh = strings.ToLower(string(*listener))
	}
	if len(route) == 0 {
		if lh == "" {
			return nil, true
		}
		return []string{lh}, true
	}
	var out []string
	for _, rh := range route {
		h := strings.TrimSuffix(strings.ToLower(string(rh)), ".")
		if lh == "" {
			out = append(out, h)
			continue
		}
		if m, ok := matchHostname(lh, h); ok {
			out = append(out, m)
		}
	}
	return dedupe(out), len(out) > 0
}

// matchHostname returns the more specific of two hostnames if they match.
func matchHostname(a, b string) (string, bool) {
	if a == b {
		return a, true
	}
	aw, bw := strings.HasPrefix(a, "*."), strings.HasPrefix(b, "*.")
	switch {
	case aw && !bw:
		if strings.HasSuffix(b, a[1:]) && len(b) > len(a)-1 {
			return b, true
		}
	case bw && !aw:
		if strings.HasSuffix(a, b[1:]) && len(a) > len(b)-1 {
			return a, true
		}
	case aw && bw:
		if strings.HasSuffix(a, b[1:]) {
			return a, true
		}
		if strings.HasSuffix(b, a[1:]) {
			return b, true
		}
	}
	return "", false
}

// HostnameAllowed reports whether host matches one of the patterns: an
// exact hostname, or "*.<domain>" for any name (or wildcard) below domain.
func HostnameAllowed(host string, patterns []string) bool {
	host = strings.ToLower(host)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(p, "*"); ok && strings.HasPrefix(suffix, ".") {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// ParsePatterns splits a comma-separated hostname pattern list.
func ParsePatterns(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---- backends ----

type backend struct {
	ns, name    string
	port        int32
	host        string // FQDN
	appProtocol string
}

// pickBackend picks the backend with the highest weight (ties by order),
// skipping weight 0. It returns a note when other backends were dropped.
func pickBackend(refs []gwv1.BackendRef) (*gwv1.BackendRef, string, string) {
	var best *gwv1.BackendRef
	bestW := int32(-1)
	nonzero := 0
	for i := range refs {
		w := int32(1)
		if refs[i].Weight != nil {
			w = *refs[i].Weight
		}
		if w <= 0 {
			continue
		}
		nonzero++
		if w > bestW {
			best, bestW = &refs[i], w
		}
	}
	switch {
	case len(refs) == 0:
		return nil, "rule has no backendRefs", ""
	case best == nil:
		return nil, "all backendRefs have weight 0", ""
	case nonzero > 1:
		return best, "", fmt.Sprintf("traffic splitting is not supported: only backend %s receives traffic", best.Name)
	}
	return best, "", ""
}

func (rc *routeCtx) resolveBackend(ref gwv1.BackendObjectReference) (*backend, bool) {
	t := rc.t
	routeNS := rc.item.ref.Namespace
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}
	kind := KindService
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	if group != "" || kind != KindService {
		rc.unresolvedf(gwv1.RouteReasonInvalidKind, "backend %s %q is not a Service", kind, ref.Name)
		return nil, false
	}
	ns := routeNS
	if ref.Namespace != nil && string(*ref.Namespace) != "" {
		ns = string(*ref.Namespace)
	}
	if ns != routeNS && !t.grantAllows(rc.item.ref.Kind, routeNS, ns, string(ref.Name)) {
		rc.unresolvedf(gwv1.RouteReasonRefNotPermitted, "backend Service %s/%s is not allowed by a ReferenceGrant", ns, ref.Name)
		return nil, false
	}
	if ref.Port == nil {
		rc.unresolvedf(gwv1.RouteReasonBackendNotFound, "backend Service %s/%s needs a port", ns, ref.Name)
		return nil, false
	}
	b := &backend{ns: ns, name: string(ref.Name), port: int32(*ref.Port)}
	if t.in.GetService != nil {
		svc := t.in.GetService(ns, string(ref.Name))
		if svc == nil {
			rc.unresolvedf(gwv1.RouteReasonBackendNotFound, "backend Service %s/%s not found", ns, ref.Name)
			return nil, false
		}
		if svc.Spec.Type == corev1.ServiceTypeExternalName {
			// Its name resolves wherever its author points it, which would
			// sidestep ReferenceGrants.
			rc.unresolvedf(gwv1.RouteReasonBackendNotFound, "backend Service %s/%s is of type ExternalName, which is not supported", ns, ref.Name)
			return nil, false
		}
		found := false
		for _, p := range svc.Spec.Ports {
			if p.Port == b.port {
				found = true
				if p.AppProtocol != nil {
					b.appProtocol = *p.AppProtocol
				}
			}
		}
		if !found {
			rc.unresolvedf(gwv1.RouteReasonBackendNotFound, "backend Service %s/%s has no port %d", ns, ref.Name, b.port)
			return nil, false
		}
	}
	domain := t.in.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	b.host = fmt.Sprintf("%s.%s.svc.%s", b.name, ns, domain)
	return b, true
}

func (t *translator) grantAllows(fromKind, fromNS, toNS, toName string) bool {
	for _, g := range t.in.ReferenceGrants {
		if g.Namespace != toNS {
			continue
		}
		fromOK := false
		for _, f := range g.Spec.From {
			if string(f.Group) == gwv1.GroupName && string(f.Kind) == fromKind && string(f.Namespace) == fromNS {
				fromOK = true
				break
			}
		}
		if !fromOK {
			continue
		}
		for _, to := range g.Spec.To {
			if string(to.Group) == "" && string(to.Kind) == KindService && (to.Name == nil || *to.Name == "" || string(*to.Name) == toName) {
				return true
			}
		}
	}
	return false
}

// TLSAppProtocol reports whether a Service port's appProtocol means the
// backend serves TLS.
func TLSAppProtocol(p string) bool { return p == "https" || p == "kubernetes.io/wss" }

// httpOrigin returns the service URL and origin options for an HTTP-like
// backend. cloudflared only speaks HTTP/1.1 to cleartext origins, so h2c
// backends are reached over HTTP/1.1.
func httpOrigin(b *backend, grpc bool) (string, *cloudflare.OriginRequest) {
	scheme := "http"
	var or *cloudflare.OriginRequest
	if TLSAppProtocol(b.appProtocol) {
		scheme = "https"
		or = &cloudflare.OriginRequest{NoTLSVerify: ptr.To(true)}
	}
	if grpc {
		if or == nil {
			or = &cloudflare.OriginRequest{}
		}
		or.HTTP2Origin = ptr.To(true)
	}
	return fmt.Sprintf("%s://%s:%d", scheme, b.host, b.port), or
}

// ---- paths ----

// ExactPathRegex matches exactly one path.
func ExactPathRegex(p string) string { return "^" + regexp.QuoteMeta(p) + "$" }

// PrefixPathRegex matches a path prefix on whole segments; "" means every
// path.
func PrefixPathRegex(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	return "^" + regexp.QuoteMeta(p) + "(?:/.*)?$"
}

// mount is a path a rule publishes, before hostnames are applied.
type mount struct {
	path     string
	kind     pathKind
	sortLen  int
	service  string
	origin   *cloudflare.OriginRequest
	ruleIdx  int
	matchIdx int
}

func pathMount(typ gwv1.PathMatchType, val string) (string, pathKind, int, error) {
	switch typ {
	case gwv1.PathMatchExact:
		return ExactPathRegex(val), pathExact, len(val), nil
	case gwv1.PathMatchPathPrefix:
		re := PrefixPathRegex(val)
		if re == "" {
			return "", pathAll, 0, nil
		}
		return re, pathPrefix, len(strings.TrimRight(val, "/")), nil
	case gwv1.PathMatchRegularExpression:
		if _, err := regexp.Compile(val); err != nil {
			return "", 0, 0, fmt.Errorf("invalid regular expression %q: %v", val, err)
		}
		return val, pathRegex, len(val), nil
	}
	return "", 0, 0, fmt.Errorf("path match type %s is not supported", typ)
}

// ---- HTTPRoute ----

func (rc *routeCtx) httpRoute(r *gwv1.HTTPRoute) {
	var mounts []mount
	usable := 0
rules:
	for i, rule := range r.Spec.Rules {
		name := ruleName(i, rule.Name)
		host := ""
		for _, f := range rule.Filters {
			if f.Type == gwv1.HTTPRouteFilterURLRewrite && f.URLRewrite != nil && f.URLRewrite.Hostname != nil && f.URLRewrite.Path == nil {
				host = string(*f.URLRewrite.Hostname)
				continue
			}
			rc.unsupportedf("%s: filter %s is not supported (only URLRewrite with a hostname)", name, f.Type)
			continue rules
		}
		if rule.Timeouts != nil || rule.Retry != nil || rule.SessionPersistence != nil {
			rc.unsupportedf("%s: timeouts, retries and session persistence are not supported", name)
			continue
		}
		type pm struct {
			path    string
			kind    pathKind
			sortLen int
			idx     int
		}
		var paths []pm
		if len(rule.Matches) == 0 {
			paths = []pm{{"", pathAll, 0, 0}}
		}
		for j, m := range rule.Matches {
			if len(m.Headers) > 0 || len(m.QueryParams) > 0 || m.Method != nil {
				rc.unsupportedf("%s: header, query parameter and method matches are not supported", name)
				continue rules
			}
			typ, val := gwv1.PathMatchPathPrefix, "/"
			if m.Path != nil {
				if m.Path.Type != nil {
					typ = *m.Path.Type
				}
				if m.Path.Value != nil {
					val = *m.Path.Value
				}
			}
			re, kind, l, err := pathMount(typ, val)
			if err != nil {
				rc.unsupportedf("%s match %d: %v", name, j, err)
				continue rules
			}
			paths = append(paths, pm{re, kind, l, j})
		}
		var refs []gwv1.BackendRef
		for _, br := range rule.BackendRefs {
			if len(br.Filters) > 0 {
				rc.unsupportedf("%s: backendRef filters are not supported", name)
				continue rules
			}
			refs = append(refs, br.BackendRef)
		}
		br, why, note := pickBackend(refs)
		if br == nil {
			rc.unsupportedf("%s: %s", name, why)
			continue
		}
		if note != "" {
			rc.notes = append(rc.notes, name+": "+note)
		}
		usable++
		service := cloudflare.UnavailableService
		var origin *cloudflare.OriginRequest
		if b, ok := rc.resolveBackend(br.BackendObjectReference); ok {
			service, origin = httpOrigin(b, false)
		}
		if host != "" && service != cloudflare.UnavailableService {
			if origin == nil {
				origin = &cloudflare.OriginRequest{}
			}
			origin.HTTPHostHeader = host
		}
		for _, p := range paths {
			mounts = append(mounts, mount{path: p.path, kind: p.kind, sortLen: p.sortLen, service: service, origin: origin, ruleIdx: i, matchIdx: p.idx})
		}
	}
	rc.nothingUsable = len(r.Spec.Rules) > 0 && usable == 0
	rc.finishHTTPLike(r.Spec.Hostnames, mounts)
}

// ---- GRPCRoute ----

func (rc *routeCtx) grpcRoute(r *gwv1.GRPCRoute) {
	var mounts []mount
	usable := 0
rules:
	for i, rule := range r.Spec.Rules {
		name := ruleName(i, rule.Name)
		if len(rule.Filters) > 0 {
			rc.unsupportedf("%s: filters are not supported", name)
			continue
		}
		if rule.SessionPersistence != nil {
			rc.unsupportedf("%s: session persistence is not supported", name)
			continue
		}
		type pm struct {
			path    string
			kind    pathKind
			sortLen int
			idx     int
		}
		var paths []pm
		if len(rule.Matches) == 0 {
			paths = []pm{{"", pathAll, 0, 0}}
		}
		for j, m := range rule.Matches {
			if len(m.Headers) > 0 {
				rc.unsupportedf("%s: header matches are not supported", name)
				continue rules
			}
			re, kind, l, err := grpcPath(m.Method)
			if err != nil {
				rc.unsupportedf("%s match %d: %v", name, j, err)
				continue rules
			}
			paths = append(paths, pm{re, kind, l, j})
		}
		var refs []gwv1.BackendRef
		for _, br := range rule.BackendRefs {
			if len(br.Filters) > 0 {
				rc.unsupportedf("%s: backendRef filters are not supported", name)
				continue rules
			}
			refs = append(refs, br.BackendRef)
		}
		br, why, note := pickBackend(refs)
		if br == nil {
			rc.unsupportedf("%s: %s", name, why)
			continue
		}
		if note != "" {
			rc.notes = append(rc.notes, name+": "+note)
		}
		usable++
		service := cloudflare.UnavailableService
		var origin *cloudflare.OriginRequest
		if b, ok := rc.resolveBackend(br.BackendObjectReference); ok {
			if TLSAppProtocol(b.appProtocol) {
				service, origin = httpOrigin(b, true)
			} else {
				rc.unresolvedf(gwv1.RouteReasonUnsupportedProtocol, "backend Service %s/%s port %d: cloudflared only uses HTTP/2 towards TLS origins, "+
					"so gRPC backends must serve TLS and set appProtocol: https on the port", b.ns, b.name, b.port)
			}
		}
		for _, p := range paths {
			mounts = append(mounts, mount{path: p.path, kind: p.kind, sortLen: p.sortLen, service: service, origin: origin, ruleIdx: i, matchIdx: p.idx})
		}
	}
	rc.nothingUsable = len(r.Spec.Rules) > 0 && usable == 0
	rc.t.grpc = true
	rc.finishHTTPLike(r.Spec.Hostnames, mounts)
}

// grpcPath turns a gRPC method match into a path regular expression; a
// call's path is /<service>/<method>.
func grpcPath(m *gwv1.GRPCMethodMatch) (string, pathKind, int, error) {
	if m == nil {
		return "", pathAll, 0, nil
	}
	svc, method := deref(m.Service), deref(m.Method)
	if m.Type != nil && *m.Type == gwv1.GRPCMethodMatchRegularExpression {
		s, me := svc, method
		if s == "" {
			s = "[^/]+"
		}
		if me == "" {
			me = "[^/]+"
		}
		for _, p := range []string{s, me} {
			if _, err := regexp.Compile(p); err != nil {
				return "", 0, 0, fmt.Errorf("invalid regular expression %q: %v", p, err)
			}
		}
		re := "^/(?:" + s + ")/(?:" + me + ")$"
		return re, pathRegex, len(re), nil
	}
	switch {
	case svc != "" && method != "":
		p := "/" + svc + "/" + method
		return ExactPathRegex(p), pathExact, len(p), nil
	case svc != "":
		re := "^/" + regexp.QuoteMeta(svc) + "/[^/]+$"
		return re, pathRegex, len(re), nil
	case method != "":
		re := "^/[^/]+/" + regexp.QuoteMeta(method) + "$"
		return re, pathRegex, len(re), nil
	}
	return "", pathAll, 0, nil
}

// ---- HTTP-like attachment ----

func (rc *routeCtx) finishHTTPLike(hostnames []gwv1.Hostname, mounts []mount) {
	t := rc.t
	for i, p := range rc.item.parents {
		if !rc.mine(p) {
			continue
		}
		atts, reason, msg := rc.attach(p, hostnames)
		if atts == nil {
			rc.record(i, rc.parentResult(p, false, reason, msg, nil, nil))
			continue
		}
		var hosts []string
		for _, a := range atts {
			a.ls.res.AttachedRoutes++
			hosts = append(hosts, a.hostnames...)
		}
		hosts = dedupe(hosts)
		if len(hosts) == 0 {
			rc.record(i, rc.parentResult(p, false, gwv1.RouteReasonUnsupportedValue,
				"the route needs a hostname (or a listener hostname): a tunnel publishes by hostname, and a rule without one would capture all traffic", nil, nil))
			continue
		}
		hosts, problems, reason := rc.filterHosts(hosts, false, atts[0].ls.set)
		if len(hosts) == 0 {
			rc.record(i, rc.parentResult(p, false, reason, strings.Join(problems, "; "), nil, nil))
			continue
		}
		var conflicts []string
		placed := 0
		for _, h := range hosts {
			for _, m := range mounts {
				if c := t.add(h, false, m, rc.item); c != "" {
					conflicts = append(conflicts, c)
				} else {
					placed++
				}
			}
		}
		if len(mounts) > 0 && placed == 0 && len(conflicts) > 0 {
			rc.record(i, rc.parentResult(p, false, ReasonConflicted, strings.Join(dedupe(conflicts), "; "), nil, nil))
			continue
		}
		rc.record(i, rc.parentResult(p, true, "", "", dedupe(append(problems, conflicts...)), hosts))
	}
}

// filterHosts applies the namespace hostname policy (of the route's
// namespace, and of the ListenerSet's when the route attaches through
// one), the claims of other Gateways and the owners within this Gateway.
// The hostnames it returns are taken for the pass's owner.
func (rc *routeCtx) filterHosts(hosts []string, tcp bool, set *setState) ([]string, []string, gwv1.RouteConditionReason) {
	t := rc.t
	var out, problems []string
	reason := gwv1.RouteConditionReason("")
	for _, h := range hosts {
		if why := t.hostnameNotAllowed(h, rc.item.ref.Namespace, rc.item.derived[h]); why != "" {
			problems = append(problems, why)
			if reason == "" {
				reason = ReasonHostnameNotAllowed
			}
			continue
		}
		if set != nil {
			if why := t.hostnameNotAllowed(h, set.ls.Namespace, false); why != "" {
				problems = append(problems, why+fmt.Sprintf(" (the namespace of ListenerSet %s/%s)", set.ls.Namespace, set.ls.Name))
				if reason == "" {
					reason = ReasonHostnameNotAllowed
				}
				continue
			}
		}
		if c, ok := t.in.Claimed[h]; ok && !containsRef(c.Routes, rc.item.ref) {
			problems = append(problems, fmt.Sprintf("hostname %s is already published by Gateway %s", h, c.Gateway))
			if reason == "" {
				reason = ReasonConflicted
			}
			continue
		}
		if o, ok := t.hostKind[h]; ok && o.tcp != tcp && o.owner != rc.item.ref {
			problems = append(problems, fmt.Sprintf("hostname %s is already published by %s", h, o.owner))
			if reason == "" {
				reason = ReasonConflicted
			}
			continue
		}
		if why := t.claim(h, rc.rank); why != "" {
			problems = append(problems, why)
			if reason == "" {
				reason = ReasonConflicted
			}
			continue
		}
		if rc.rank == 0 && strings.HasPrefix(h, "*.") {
			t.gatewayWildcards[h] = true
		}
		out = append(out, h)
	}
	return out, problems, reason
}

// hostnameNotAllowed checks a hostname against a namespace's
// allowed-hostnames policy; derived hostnames are exempt.
func (t *translator) hostnameNotAllowed(h, ns string, derived bool) string {
	if t.in.AllowedHostnames == nil || derived {
		return ""
	}
	patterns := t.in.AllowedHostnames(ns)
	if patterns == nil || HostnameAllowed(h, patterns) {
		return ""
	}
	return fmt.Sprintf("hostname %s is not allowed in namespace %s", h, ns)
}

func containsRef(refs []RouteRef, r RouteRef) bool {
	for _, x := range refs {
		if x == r {
			return true
		}
	}
	return false
}

// add places one rule; it returns a conflict message or "".
func (t *translator) add(host string, tcp bool, m mount, it *routeItem) string {
	key := ruleKey{host, m.path}
	if tcp {
		key.path = "\x00tcp"
	}
	if e, ok := t.byKey[key]; ok {
		if e.owner == it.ref {
			return ""
		}
		p := m.path
		if p == "" {
			p = "every path"
		}
		return fmt.Sprintf("%s (%s) is already published by %s", host, p, e.owner)
	}
	if _, ok := t.hostKind[host]; !ok {
		t.hostKind[host] = hostOwner{tcp: tcp, owner: it.ref}
	}
	e := &entry{
		rule:     cloudflare.IngressRule{Hostname: host, Path: m.path, Service: m.service, OriginRequest: m.origin},
		tcp:      tcp,
		kind:     m.kind,
		sortLen:  m.sortLen,
		owner:    it.ref,
		created:  it.created,
		ruleIdx:  m.ruleIdx,
		matchIdx: m.matchIdx,
		dnsPar:   it.dnsParent,
	}
	if it.dnsParent != "" {
		for _, p := range it.parents {
			ns := it.ref.Namespace
			if p.Namespace != nil {
				ns = string(*p.Namespace)
			}
			if (p.Group == nil || *p.Group == gwv1.GroupName) && (p.Kind == nil || *p.Kind == "Gateway") {
				e.dnsFrom = append(e.dnsFrom, ns+"/"+string(p.Name))
			}
		}
	}
	t.byKey[key] = e
	t.entries = append(t.entries, e)
	return ""
}

// ---- TCPRoute ----

// TCPHostname returns the hostname a TCPRoute publishes, from its hostname
// annotation (Gateway API TCP listeners cannot carry hostnames), or why it
// has none.
func TCPHostname(r *gwv1.TCPRoute) (string, string) {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Annotations[config.AnnotationHostname]), "."))
	switch {
	case h == "":
		return "", "a TCPRoute names the hostname it publishes with annotation " + config.AnnotationHostname
	case strings.Contains(h, "*") || len(validation.IsDNS1123Subdomain(h)) > 0 || !strings.Contains(h, "."):
		return "", fmt.Sprintf("annotation %s: %q is not a hostname (wildcards are not allowed)", config.AnnotationHostname, h)
	}
	return h, ""
}

func (rc *routeCtx) tcpRoute(r *gwv1.TCPRoute) {
	t := rc.t
	var service string
	switch {
	case len(r.Spec.Rules) == 0:
		rc.unsupportedf("route has no rules")
		rc.nothingUsable = true
	default:
		if len(r.Spec.Rules) > 1 {
			rc.notes = append(rc.notes, "only the first rule is used")
		}
		br, why, note := pickBackend(r.Spec.Rules[0].BackendRefs)
		if br == nil {
			rc.unsupportedf("rule 0: %s", why)
			rc.nothingUsable = true
			break
		}
		if note != "" {
			rc.notes = append(rc.notes, note)
		}
		service = cloudflare.UnavailableService
		if b, ok := rc.resolveBackend(br.BackendObjectReference); ok {
			service = fmt.Sprintf("tcp://%s:%d", b.host, b.port)
		}
	}
	for i, p := range rc.item.parents {
		if !rc.mine(p) {
			continue
		}
		atts, reason, msg := rc.attach(p, nil)
		if atts == nil {
			rc.record(i, rc.parentResult(p, false, reason, msg, nil, nil))
			continue
		}
		for _, a := range atts {
			a.ls.res.AttachedRoutes++
		}
		host, bad := TCPHostname(r)
		if bad != "" {
			rc.record(i, rc.parentResult(p, false, gwv1.RouteReasonUnsupportedValue, bad, nil, nil))
			continue
		}
		hosts, problems, reason := rc.filterHosts([]string{host}, true, atts[0].ls.set)
		if len(hosts) == 0 {
			rc.record(i, rc.parentResult(p, false, reason, strings.Join(problems, "; "), nil, nil))
			continue
		}
		var conflicts []string
		placed := 0
		if service != "" {
			for _, h := range hosts {
				if c := t.add(h, true, mount{service: service, kind: pathAll}, rc.item); c != "" {
					conflicts = append(conflicts, c)
				} else {
					placed++
				}
			}
		}
		if service != "" && placed == 0 && len(conflicts) > 0 {
			rc.record(i, rc.parentResult(p, false, ReasonConflicted, strings.Join(dedupe(conflicts), "; "), nil, nil))
			continue
		}
		rc.record(i, rc.parentResult(p, true, "", "", dedupe(append(problems, conflicts...)), hosts))
	}
}

// ---- status ----

func (rc *routeCtx) parentResult(p gwv1.ParentReference, attached bool, reason gwv1.RouteConditionReason, msg string, extra, hosts []string) ParentResult {
	g := rc.item.generation
	var conds []metav1.Condition
	switch {
	case !attached:
		conds = append(conds, cond(gwv1.RouteConditionAccepted, false, reason, msg, g))
		hosts = nil
	case rc.nothingUsable:
		conds = append(conds, cond(gwv1.RouteConditionAccepted, false, gwv1.RouteReasonUnsupportedValue,
			"no rule can be published: "+strings.Join(rc.unsupported, "; "), g))
		hosts = nil
	default:
		m := "Route accepted"
		if len(rc.notes) > 0 {
			m += "; " + strings.Join(rc.notes, "; ")
		}
		conds = append(conds, cond(gwv1.RouteConditionAccepted, true, gwv1.RouteReasonAccepted, m, g))
		problems := append(append([]string{}, rc.unsupported...), extra...)
		if len(problems) > 0 {
			conds = append(conds, cond(gwv1.RouteConditionPartiallyInvalid, true, gwv1.RouteReasonUnsupportedValue, strings.Join(problems, "; "), g))
		}
	}
	if len(rc.unresolved) > 0 {
		conds = append(conds, cond(gwv1.RouteConditionResolvedRefs, false, rc.resolvedWhy, strings.Join(rc.unresolved, "; "), g))
	} else {
		conds = append(conds, cond(gwv1.RouteConditionResolvedRefs, true, gwv1.RouteReasonResolvedRefs, "References resolved", g))
	}
	return ParentResult{ParentRef: p, Conditions: conds, Hostnames: hosts}
}

// ---- rendering ----

func wildcardRank(h string) (bool, int) {
	if strings.HasPrefix(h, "*.") {
		return true, len(h)
	}
	return false, len(h)
}

func less(a, b *entry) bool {
	if a.tcp != b.tcp {
		return a.tcp
	}
	aw, al := wildcardRank(a.rule.Hostname)
	bw, bl := wildcardRank(b.rule.Hostname)
	if aw != bw {
		return !aw
	}
	if aw && al != bl {
		return al > bl
	}
	if a.rule.Hostname != b.rule.Hostname {
		return a.rule.Hostname < b.rule.Hostname
	}
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	if a.sortLen != b.sortLen {
		return a.sortLen > b.sortLen
	}
	if !a.created.Equal(&b.created) {
		return a.created.Before(&b.created)
	}
	if a.owner != b.owner {
		return a.owner.String() < b.owner.String()
	}
	if a.ruleIdx != b.ruleIdx {
		return a.ruleIdx < b.ruleIdx
	}
	return a.matchIdx < b.matchIdx
}

// DefaultOriginRequest is the tunnel-wide origin request configuration.
func DefaultOriginRequest() *cloudflare.OriginRequest {
	return &cloudflare.OriginRequest{KeepAliveConnections: ptr.To(-1)}
}

func (t *translator) render() (*cloudflare.TunnelConfig, []Hostname) {
	sort.SliceStable(t.entries, func(i, j int) bool { return less(t.entries[i], t.entries[j]) })
	cfg := &cloudflare.TunnelConfig{OriginRequest: DefaultOriginRequest()}
	hosts := map[string]*Hostname{}
	var order []string
	for _, e := range t.entries {
		cfg.Ingress = append(cfg.Ingress, e.rule)
		h := hosts[e.rule.Hostname]
		if h == nil {
			h = &Hostname{Name: e.rule.Hostname, TCP: e.tcp}
			hosts[e.rule.Hostname] = h
			order = append(order, e.rule.Hostname)
		}
		if !containsRef(h.Routes, e.owner) {
			h.Routes = append(h.Routes, e.owner)
		}
		if e.dnsPar != "" && h.DNSParent == "" {
			h.DNSParent, h.DNSFrom = e.dnsPar, e.dnsFrom
		}
	}
	cfg.Ingress = append(cfg.Ingress, cloudflare.IngressRule{Service: cloudflare.NotFoundService})
	sort.Strings(order)
	var out []Hostname
	for _, n := range order {
		out = append(out, *hosts[n])
	}
	return cfg, out
}

// ---- helpers ----

func cond[T ~string, R ~string](typ T, status bool, reason R, msg string, gen int64) metav1.Condition {
	s := metav1.ConditionFalse
	if status {
		s = metav1.ConditionTrue
	}
	if len(msg) > 32000 {
		msg = msg[:32000]
	}
	return metav1.Condition{Type: string(typ), Status: s, Reason: string(reason), Message: msg, ObservedGeneration: gen}
}

// Cond builds a condition (exported for the controllers).
func Cond[T ~string, R ~string](typ T, status bool, reason R, msg string, gen int64) metav1.Condition {
	return cond(typ, status, reason, msg, gen)
}

func ruleName(i int, n *gwv1.SectionName) string {
	if n != nil && *n != "" {
		return fmt.Sprintf("rule %q", *n)
	}
	return "rule " + strconv.Itoa(i)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
