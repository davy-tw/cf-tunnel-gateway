package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// generator holds what the Ingress and Service translators share: they
// write routes (and dedicated Gateways) owned by a source object and
// attach them to a parent Gateway.
type generator struct {
	classResolver
	Scheme   *runtime.Scheme
	Features *Features
	Recorder events.EventRecorder
	Now      func() time.Time
}

func (g *generator) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *generator) event(obj runtime.Object, typ, reason, msg string, args ...any) {
	if g.Recorder != nil {
		g.Recorder.Eventf(obj, nil, typ, reason, "Translate", msg, args...)
	}
}

func (g *generator) warn(obj runtime.Object, reason, msg string, args ...any) {
	g.event(obj, corev1.EventTypeWarning, reason, msg, args...)
}

// source is an Ingress or Service being translated.
type sourceObj struct {
	obj        client.Object
	kind       string // Ingress or Service
	apiVersion string
	label      string // config.SourceIngress or config.SourceService
}

func (s sourceObj) key() string { return s.obj.GetNamespace() + "/" + s.obj.GetName() }

// meta is the metadata of an object generated for the source.
func (s sourceObj) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   s.obj.GetNamespace(),
		Annotations: map[string]string{config.AnnotationSource: s.label},
		Labels:      map[string]string{config.LabelSourceKind: s.label, config.LabelSourceName: LabelValue(s.obj.GetName())},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: s.apiVersion, Kind: s.kind, Name: s.obj.GetName(), UID: s.obj.GetUID(),
			Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}},
	}
}

// LabelValue fits a name into a label value.
func LabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}
	return name[:46] + "-" + hashString(name)
}

// conflictRequeue is how often a source whose generated names are taken
// checks again: deleting the object in the way does not enqueue the
// source, since the source does not own it.
const conflictRequeue = time.Minute

// applyGenerated applies obj unless an object with that name exists and is
// not controlled by owner.
func (g *generator) applyGenerated(ctx context.Context, owner client.Object, obj client.Object) error {
	return applyControlled(ctx, g.Client, g.reader(), g.Scheme, obj, owner)
}

// generated lists the objects of list's kind generated for src.
func (g *generator) generated(ctx context.Context, src sourceObj, list client.ObjectList) ([]client.Object, error) {
	if err := g.List(ctx, list, client.InNamespace(src.obj.GetNamespace()),
		client.MatchingLabels{config.LabelSourceKind: src.label, config.LabelSourceName: LabelValue(src.obj.GetName())}); err != nil {
		if apierrors.IsNotFound(err) || isNoKindMatch(err) {
			return nil, nil
		}
		return nil, err
	}
	items, err := listItems(list)
	if err != nil {
		return nil, err
	}
	var out []client.Object
	for _, o := range items {
		if metav1.IsControlledBy(o, src.obj) {
			out = append(out, o)
		}
	}
	return out, nil
}

// pruneGenerated deletes objects of list's kind generated for src whose
// names are not in keep.
func (g *generator) pruneGenerated(ctx context.Context, src sourceObj, list client.ObjectList, keep map[string]bool) error {
	items, err := g.generated(ctx, src, list)
	if err != nil {
		return err
	}
	for _, o := range items {
		if keep[o.GetName()] {
			continue
		}
		if err := g.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func listItems(list client.ObjectList) ([]client.Object, error) {
	var out []client.Object
	switch l := list.(type) {
	case *gwv1.GatewayList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *gwv1.HTTPRouteList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	case *gwv1.TCPRouteList:
		for i := range l.Items {
			out = append(out, &l.Items[i])
		}
	default:
		return nil, fmt.Errorf("unsupported list %T", list)
	}
	return out, nil
}

// cleanupSource deletes everything generated for src.
func (g *generator) cleanupSource(ctx context.Context, src sourceObj) error {
	if err := g.pruneGenerated(ctx, src, &gwv1.HTTPRouteList{}, nil); err != nil {
		return err
	}
	if g.Features.TCPRoute() {
		if err := g.pruneGenerated(ctx, src, &gwv1.TCPRouteList{}, nil); err != nil {
			return err
		}
	}
	return g.pruneGenerated(ctx, src, &gwv1.GatewayList{}, nil)
}

// ---- parents ----

type parentKind int

const (
	parentShared parentKind = iota
	parentDedicated
	parentExplicit
)

// parent is a Gateway generated routes attach to.
type parent struct {
	Namespace, Name string
	Kind            parentKind
}

func (p parent) key() string { return p.Namespace + "/" + p.Name }

// SharedKey returns the shared Gateway for a namespace.
func SharedKey(cfg *config.Config, ns string) types.NamespacedName {
	if cfg.SharedScope == config.ScopeCluster {
		return types.NamespacedName{Namespace: cfg.SharedNamespace, Name: cfg.SharedName}
	}
	return types.NamespacedName{Namespace: ns, Name: cfg.SharedName}
}

// desiredParent reads a source's gateway annotation.
func (g *generator) desiredParent(src sourceObj) (parent, error) {
	v := strings.TrimSpace(src.obj.GetAnnotations()[config.AnnotationGateway])
	switch v {
	case "", config.GatewayShared:
		k := SharedKey(g.Config, src.obj.GetNamespace())
		return parent{Namespace: k.Namespace, Name: k.Name, Kind: parentShared}, nil
	case config.GatewayDedicated:
		return parent{Namespace: src.obj.GetNamespace(), Name: src.obj.GetName(), Kind: parentDedicated}, nil
	}
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" {
		return parent{}, fmt.Errorf("annotation %s: %q must be %s, %s or <namespace>/<name>", config.AnnotationGateway, v, config.GatewayShared, config.GatewayDedicated)
	}
	return parent{Namespace: ns, Name: name, Kind: parentExplicit}, nil
}

// parentOf classifies an existing parent key of src's routes.
func (g *generator) parentOf(ctx context.Context, src sourceObj, key string) parent {
	ns, name, _ := strings.Cut(key, "/")
	p := parent{Namespace: ns, Name: name, Kind: parentExplicit}
	gw := &gwv1.Gateway{}
	if err := g.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, gw); err == nil {
		switch {
		case metav1.IsControlledBy(gw, src.obj):
			p.Kind = parentDedicated
		case gw.Annotations[config.AnnotationManaged] == config.ManagedShared:
			p.Kind = parentShared
		}
	} else if ns == src.obj.GetNamespace() && name == src.obj.GetName() {
		p.Kind = parentDedicated
	} else if k := SharedKey(g.Config, src.obj.GetNamespace()); k.Namespace == ns && k.Name == name {
		p.Kind = parentShared
	}
	return p
}

// TCPListenerName names the TCP listener of shared and dedicated
// Gateways. Gateway API TCP listeners cannot carry hostnames, so one
// listener takes every TCP route; each route names its own hostname.
const TCPListenerName gwv1.SectionName = "tcp"

// TCPListenerPort is the port of that listener. Nothing listens on it: a
// tunnel publishes TCP by hostname, and clients connect through
// `cloudflared access tcp`.
const TCPListenerPort = 1

// parentRef points a generated route at a parent. HTTP routes reach a
// shared or dedicated Gateway through port 80 (every HTTP listener there),
// TCP routes through its TCP listener.
func parentRef(p parent, tcpHost string) gwv1.ParentReference {
	ref := gatewayParentRef(p.Namespace, p.Name)
	switch {
	case tcpHost != "":
		ref.SectionName = ptr.To(TCPListenerName)
	case p.Kind != parentExplicit:
		ref.Port = ptr.To(gwv1.PortNumber(80))
	}
	return ref
}

// parentProblem reports why a desired parent cannot take routes yet, or "".
func (g *generator) parentProblem(ctx context.Context, src sourceObj, p parent) string {
	gw := &gwv1.Gateway{}
	err := g.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, gw)
	switch p.Kind {
	case parentShared:
		if err == nil && gw.Annotations[config.AnnotationManaged] != config.ManagedShared {
			return fmt.Sprintf("Gateway %s exists but is not a shared Gateway of this controller (annotation %s: %s); it is left alone",
				p.key(), config.AnnotationManaged, config.ManagedShared)
		}
	case parentExplicit:
		if err != nil {
			return fmt.Sprintf("Gateway %s: %v", p.key(), err)
		}
		if gc, _ := g.managedClass(ctx, string(gw.Spec.GatewayClassName)); gc == nil {
			return fmt.Sprintf("Gateway %s is not of a class served by this controller", p.key())
		}
	}
	return ""
}

// gatewayProgrammed reports whether a Gateway is programmed for its
// current generation, and its tunnel target.
func (g *generator) gatewayProgrammed(ctx context.Context, key string) (bool, string) {
	ns, name, _ := strings.Cut(key, "/")
	gw := &gwv1.Gateway{}
	if err := g.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, gw); err != nil {
		return false, ""
	}
	if !conditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed), gw.Generation) {
		return false, ""
	}
	for _, a := range gw.Status.Addresses {
		if a.Type != nil && *a.Type == gwv1.HostnameAddressType {
			return true, a.Value
		}
	}
	return true, ""
}

// ---- attaching routes ----

// genRoute is a generated route before its parent is known.
type genRoute struct {
	obj client.Object // *gwv1.HTTPRoute or *gwv1.TCPRoute
	// tcpHost is the hostname of a TCP route.
	tcpHost string
}

func routeParentRefs(o client.Object) *[]gwv1.ParentReference {
	switch r := o.(type) {
	case *gwv1.HTTPRoute:
		return &r.Spec.ParentRefs
	case *gwv1.TCPRoute:
		return &r.Spec.ParentRefs
	}
	return nil
}

func routeStatusParents(o client.Object) []gwv1.RouteParentStatus {
	switch r := o.(type) {
	case *gwv1.HTTPRoute:
		return r.Status.Parents
	case *gwv1.TCPRoute:
		return r.Status.Parents
	}
	return nil
}

// Switch phases, recorded on generated routes as "<phase>@<RFC 3339 time>".
const (
	phaseWaiting = "waiting" // for the new parent to be programmed
	phaseDNS     = "dns"     // for the new parent to own the DNS records
	phaseGrace   = "grace"   // before the old parent is dropped
)

func parseSwitch(v string) (string, time.Time) {
	phase, at, ok := strings.Cut(v, "@")
	if !ok {
		return "", time.Time{}
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return "", time.Time{}
	}
	return phase, t
}

// attachment is the outcome of attach.
type attachment struct {
	// serving is the parent whose tunnel serves the source's hostnames
	// now, for the source's status.
	serving string
	requeue time.Duration
	// conflicted is set when a generated name is taken.
	conflicted bool
	// keepOld is the dedicated Gateway to keep while switching away from it.
	keepOld string
}

// attach points src's routes at the desired parent, switching from the
// current one make-before-break, and applies them. Existing routes of
// other names are pruned by the caller.
func (g *generator) attach(ctx context.Context, src sourceObj, desired parent, routes []genRoute) (*attachment, error) {
	var existing []client.Object
	for _, l := range []client.ObjectList{&gwv1.HTTPRouteList{}, &gwv1.TCPRouteList{}} {
		if _, tcp := l.(*gwv1.TCPRouteList); tcp && !g.Features.TCPRoute() {
			continue
		}
		items, err := g.generated(ctx, src, l)
		if err != nil {
			return nil, err
		}
		existing = append(existing, items...)
	}
	current := map[string]bool{}
	switchAnn, dnsParent := "", ""
	for _, o := range existing {
		for _, p := range *routeParentRefs(o) {
			ns := o.GetNamespace()
			if p.Namespace != nil {
				ns = string(*p.Namespace)
			}
			current[ns+"/"+string(p.Name)] = true
		}
		if v := o.GetAnnotations()[config.AnnotationSwitch]; v != "" {
			switchAnn = v
		}
		if v := o.GetAnnotations()[config.AnnotationDNSParent]; v != "" {
			dnsParent = v
		}
	}
	want := desired.key()
	out := &attachment{serving: want}
	refs := []parent{desired}
	ann := map[string]string{config.AnnotationGateway: want}

	if len(current) > 0 && !(len(current) == 1 && current[want]) {
		// Switching. The old parent is the one that is not desired; with
		// two others (the desired parent changed mid-switch), the one that
		// was taking over DNS.
		old := ""
		keys := sortedKeys(current)
		for _, k := range keys {
			if k != want {
				old = k
				break
			}
		}
		if len(keys) == 2 && !current[want] && dnsParent != "" && current[dnsParent] {
			old = dnsParent
			switchAnn = ""
		}
		oldParent := g.parentOf(ctx, src, old)
		if oldParent.Kind == parentDedicated {
			out.keepOld = old
		}
		phase, at := parseSwitch(switchAnn)
		if dnsParent != "" && dnsParent != want {
			phase = ""
		}
		now := g.now()
		if phase == "" {
			phase, at = phaseWaiting, now
			g.event(src.obj, corev1.EventTypeNormal, "Switching", "moving from Gateway %s to %s: waiting for %s to be programmed", old, want, want)
		}
		timedOut := now.Sub(at) > g.Config.SwitchTimeout
		switch phase {
		case phaseWaiting:
			out.serving, refs = old, []parent{oldParent}
			if ok, _ := g.gatewayProgrammed(ctx, want); ok {
				phase, at = phaseDNS, now
				refs = []parent{oldParent, desired}
				g.event(src.obj, corev1.EventTypeNormal, "Switching", "Gateway %s is programmed; moving DNS records from %s", want, old)
			} else if timedOut {
				g.warn(src.obj, "SwitchTimeout", "Gateway %s is not programmed after %s; staying on %s and trying again", want, g.Config.SwitchTimeout, old)
				at = now
			}
			out.requeue = pendingRequeue
		case phaseDNS:
			out.serving, refs = old, []parent{oldParent, desired}
			if g.dnsMoved(existing, want) {
				phase, at = phaseGrace, now
				out.serving = want
				out.requeue = g.Config.SwitchGrace
			} else {
				if timedOut {
					g.warn(src.obj, "SwitchTimeout", "DNS records are not yet on Gateway %s after %s; still waiting", want, g.Config.SwitchTimeout)
					at = now
				}
				out.requeue = pendingRequeue
			}
		case phaseGrace:
			refs = []parent{oldParent, desired}
			if left := g.Config.SwitchGrace - now.Sub(at); left > 0 {
				out.requeue = left
			} else {
				refs = []parent{desired}
				out.keepOld = ""
				phase = ""
				g.event(src.obj, corev1.EventTypeNormal, "Switched", "now served by Gateway %s", want)
			}
		}
		if phase != "" {
			ann[config.AnnotationSwitch] = phase + "@" + at.UTC().Format(time.RFC3339)
			if len(refs) == 2 {
				ann[config.AnnotationDNSParent] = want
			}
		}
	}

	for _, rt := range routes {
		var prs []gwv1.ParentReference
		for _, p := range refs {
			prs = append(prs, parentRef(p, rt.tcpHost))
		}
		*routeParentRefs(rt.obj) = prs
		a := rt.obj.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		for k, v := range ann {
			a[k] = v
		}
		rt.obj.SetAnnotations(a)
		if err := g.applyGenerated(ctx, src.obj, rt.obj); err != nil {
			if ce, ok := err.(*conflictError); ok {
				g.warn(src.obj, "NameConflict", "%s", ce.msg)
				out.conflicted = true
				continue
			}
			return nil, err
		}
	}
	return out, nil
}

// dnsMoved reports whether every existing route attached to parent has its
// DNS records there.
func (g *generator) dnsMoved(routes []client.Object, parentKey string) bool {
	ns, name, _ := strings.Cut(parentKey, "/")
	for _, o := range routes {
		found := false
		for _, ps := range routeStatusParents(o) {
			if string(ps.ControllerName) != g.Config.ControllerName || !translate.ParentTargets(ps.ParentRef, o.GetNamespace(), ns, name) {
				continue
			}
			found = true
			if !conditionTrue(ps.Conditions, string(gwv1.RouteConditionAccepted), o.GetGeneration()) {
				return false
			}
			if c := meta.FindStatusCondition(ps.Conditions, translate.ConditionDNSRecordsReady); c != nil && c.Status != metav1.ConditionTrue {
				return false
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ---- dedicated Gateways ----

// dedicatedGateway is the Gateway generated for a source that asked for
// its own tunnel.
func (g *generator) dedicatedGateway(ctx context.Context, src sourceObj, tcpHosts []tcpListener) (*gwv1.Gateway, error) {
	pol, err := namespacePolicy(ctx, g.Client, g.Config, src.obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	gw := &gwv1.Gateway{
		ObjectMeta: src.meta(src.obj.GetName()),
		Spec: gwv1.GatewaySpec{
			GatewayClassName: gwv1.ObjectName(generatedClass(g.Config, pol)),
			Listeners: []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80,
				AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(gwv1.NamespacesFromSame)}}}},
		},
	}
	gw.Spec.Listeners = append(gw.Spec.Listeners, tcpListeners(tcpHosts, gwv1.NamespacesFromSame)...)
	if cm := strings.TrimSpace(src.obj.GetAnnotations()[config.AnnotationParameters]); cm != "" {
		gw.Spec.Infrastructure = &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{Group: "", Kind: "ConfigMap", Name: cm}}
	}
	return gw, nil
}

// tcpListener is a TCP hostname a source publishes.
type tcpListener struct {
	Host      string
	Port      int32
	Namespace string
}

// tcpListeners returns the TCP listener when any TCP hostname is
// published; from is who may attach.
func tcpListeners(hosts []tcpListener, from gwv1.FromNamespaces) []gwv1.Listener {
	if len(hosts) == 0 {
		return nil
	}
	return []gwv1.Listener{{
		Name: TCPListenerName, Protocol: gwv1.TCPProtocolType, Port: TCPListenerPort,
		AllowedRoutes: &gwv1.AllowedRoutes{
			Kinds:      []gwv1.RouteGroupKind{{Group: ptr.To(gwv1.Group(gwv1.GroupName)), Kind: translate.KindTCPRoute}},
			Namespaces: &gwv1.RouteNamespaces{From: ptr.To(from)},
		},
	}}
}

// sourceStatusTarget returns the tunnel target that serves the source, or
// "" while its parent is not programmed.
func (g *generator) sourceStatusTarget(ctx context.Context, a *attachment) string {
	if a == nil {
		return ""
	}
	ok, target := g.gatewayProgrammed(ctx, a.serving)
	if !ok {
		return ""
	}
	return target
}

// syncSource writes everything generated for src: its routes attached to
// the desired parent, and its dedicated Gateway when it has one. It
// returns nil when the routes were left as they were.
func (g *generator) syncSource(ctx context.Context, src sourceObj, routes []genRoute, tcpHosts []tcpListener) (*attachment, error) {
	if len(routes) == 0 {
		return nil, g.cleanupSource(ctx, src)
	}
	desired, err := g.desiredParent(src)
	if err != nil {
		g.warn(src.obj, "InvalidAnnotation", "%v", err)
		return &attachment{requeue: conflictRequeue}, nil
	}
	if problem := g.parentProblem(ctx, src, desired); problem != "" {
		g.warn(src.obj, "GatewayUnavailable", "%s", problem)
		return &attachment{requeue: conflictRequeue}, nil
	}
	if desired.Kind == parentExplicit && len(tcpHosts) > 0 {
		g.warn(src.obj, "TCPNotSupported", "TCP hostnames need a shared or dedicated Gateway; they are not published through %s", desired.key())
		var httpOnly []genRoute
		for _, rt := range routes {
			if rt.tcpHost == "" {
				httpOnly = append(httpOnly, rt)
			}
		}
		routes, tcpHosts = httpOnly, nil
	}
	if desired.Kind == parentDedicated {
		gw, err := g.dedicatedGateway(ctx, src, tcpHosts)
		if err != nil {
			return nil, err
		}
		if err := g.applyGenerated(ctx, src.obj, gw); err != nil {
			if ce, ok := err.(*conflictError); ok {
				g.warn(src.obj, "NameConflict", "%s", ce.msg)
				return &attachment{requeue: conflictRequeue, conflicted: true}, nil
			}
			return nil, err
		}
	}
	a, err := g.attach(ctx, src, desired, routes)
	if err != nil {
		return nil, err
	}
	keepHTTP, keepTCP := map[string]bool{}, map[string]bool{}
	for _, rt := range routes {
		if rt.tcpHost != "" {
			keepTCP[rt.obj.GetName()] = true
		} else {
			keepHTTP[rt.obj.GetName()] = true
		}
	}
	if err := g.pruneGenerated(ctx, src, &gwv1.HTTPRouteList{}, keepHTTP); err != nil {
		return nil, err
	}
	if g.Features.TCPRoute() {
		if err := g.pruneGenerated(ctx, src, &gwv1.TCPRouteList{}, keepTCP); err != nil {
			return nil, err
		}
	}
	keepGW := map[string]bool{}
	if desired.Kind == parentDedicated || a.keepOld == src.key() {
		keepGW[src.obj.GetName()] = true
	}
	if err := g.pruneGenerated(ctx, src, &gwv1.GatewayList{}, keepGW); err != nil {
		return nil, err
	}
	if a.conflicted && a.requeue == 0 {
		a.requeue = conflictRequeue
	}
	return a, nil
}
