package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

const annotationDefaultIngressClass = "ingressclass.kubernetes.io/is-default-class"

// annotationLegacyIngressClass is the deprecated way to pick a class. The
// API server does not default spec.ingressClassName for Ingresses that set
// it.
const annotationLegacyIngressClass = "kubernetes.io/ingress.class"

// IngressReconciler translates Ingresses of the configured IngressClass into
// HTTPRoutes on a shared or dedicated Gateway.
type IngressReconciler struct {
	generator
}

// NewIngressReconciler returns a reconciler.
func NewIngressReconciler(c client.Client, scheme *runtime.Scheme, cfg *config.Config, cf *cloudflare.Client, f *Features, rec events.EventRecorder) *IngressReconciler {
	return &IngressReconciler{generator: generator{classResolver: classResolver{Client: c, Config: cfg, CF: cf}, Scheme: scheme, Features: f, Recorder: rec}}
}

func ingressSource(ing *networkingv1.Ingress) sourceObj {
	return sourceObj{obj: ing, kind: "Ingress", apiVersion: "networking.k8s.io/v1", label: config.SourceIngress}
}

// managed reports whether the Ingress belongs to this controller: its
// class (named, or the cluster default when none is named) is the
// configured IngressClass, and that class names this controller. An
// Ingress without spec.ingressClassName that names a class through the
// legacy annotation belongs to that class, never to the default one.
func (r *IngressReconciler) managed(ctx context.Context, ing *networkingv1.Ingress) (bool, error) {
	named := ing.Spec.IngressClassName != nil
	if named && *ing.Spec.IngressClassName != r.Config.IngressClass {
		return false, nil
	}
	if legacy, ok := ing.Annotations[annotationLegacyIngressClass]; !named && ok {
		if legacy != r.Config.IngressClass {
			return false, nil
		}
		named = true
	}
	ic := &networkingv1.IngressClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.Config.IngressClass}, ic); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if ic.Spec.Controller != config.IngressControllerName {
		return false, nil
	}
	return named || ic.Annotations[annotationDefaultIngressClass] == "true", nil
}

// Reconcile implements reconcile.Reconciler.
func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ing); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	src := ingressSource(ing)
	managed, err := r.managed(ctx, ing)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed || !ing.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanupSource(ctx, src)
	}
	routes, err := r.translate(ctx, ing)
	if err != nil {
		return ctrl.Result{}, ignoreRateLimit(err)
	}
	if len(ing.Spec.TLS) > 0 {
		r.event(ing, corev1.EventTypeNormal, "TLSIgnored", "spec.tls is not used: Cloudflare serves the hostnames with its edge certificates")
	}
	a, err := r.syncSource(ctx, src, routes, nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	res := ctrl.Result{}
	target := ""
	if a != nil {
		res.RequeueAfter = a.requeue
		if a.serving == "" {
			return res, nil // routes left alone
		}
		target = r.sourceStatusTarget(ctx, a)
	}
	return res, r.writeStatus(ctx, ing, target)
}

func (r *IngressReconciler) writeStatus(ctx context.Context, ing *networkingv1.Ingress, target string) error {
	var lbs []networkingv1.IngressLoadBalancerIngress
	if target != "" {
		lbs = []networkingv1.IngressLoadBalancerIngress{{Hostname: target}}
	}
	if reflect.DeepEqual(lbs, ing.Status.LoadBalancer.Ingress) || len(lbs) == 0 && len(ing.Status.LoadBalancer.Ingress) == 0 {
		return nil
	}
	orig := ing.DeepCopy()
	ing.Status.LoadBalancer.Ingress = lbs
	return r.Status().Patch(ctx, ing, client.MergeFrom(orig))
}

// translate builds the HTTPRoutes of an Ingress, one per host, so that
// each host only gets its own paths.
func (r *IngressReconciler) translate(ctx context.Context, ing *networkingv1.Ingress) ([]genRoute, error) {
	src := ingressSource(ing)
	type group struct {
		host  string
		rules []gwv1.HTTPRouteRule
		// broken groups have a path whose backend cannot be used. They
		// are not served at all: leaving the path out would send its
		// requests to a shorter prefix that belongs to another backend.
		broken bool
	}
	var groups []*group
	byHost := map[string]*group{}
	for _, rule := range ing.Spec.Rules {
		host := strings.ToLower(rule.Host)
		g := byHost[host]
		if g == nil {
			g = &group{host: host}
			byHost[host] = g
			groups = append(groups, g)
		}
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			be, ok := r.backendRef(ctx, ing, p.Backend)
			if !ok {
				g.broken = true
				continue
			}
			path := p.Path
			if path == "" {
				path = "/"
			}
			typ := gwv1.PathMatchPathPrefix
			if p.PathType != nil && *p.PathType == networkingv1.PathTypeExact {
				typ = gwv1.PathMatchExact
			}
			g.rules = append(g.rules, gwv1.HTTPRouteRule{
				Matches:     []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: ptr.To(typ), Value: ptr.To(path)}}},
				BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: be}},
			})
		}
	}
	// The default backend takes every request that no rule matches: the
	// rest of each host's paths, and the derived hostname.
	if ing.Spec.DefaultBackend != nil {
		if be, ok := r.backendRef(ctx, ing, *ing.Spec.DefaultBackend); ok {
			fallback := gwv1.HTTPRouteRule{
				Matches:     []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: ptr.To(gwv1.PathMatchPathPrefix), Value: ptr.To("/")}}},
				BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: be}},
			}
			if byHost[""] == nil {
				g := &group{}
				byHost[""] = g
				groups = append(groups, g)
			}
			for _, g := range groups {
				if !hasRootPrefix(g.rules) {
					g.rules = append(g.rules, fallback)
				}
			}
		} else if g := byHost[""]; g != nil {
			g.broken = true
		}
	}
	var routes []genRoute
	for i, g := range groups {
		label := g.host
		if label == "" {
			label = "(no host)"
		}
		if g.broken {
			r.warn(ing, "HostNotServed", "host %s is not served until all of its backends resolve", label)
			continue
		}
		if len(g.rules) == 0 {
			continue
		}
		host, derived := g.host, ""
		if host == "" {
			h, why, err := r.derivedHostname(ctx, src, "")
			if err != nil {
				return nil, err
			}
			if h == "" {
				r.warn(ing, "NoHostname", "rules without a host are not served: a tunnel publishes by hostname only, and %s", why)
				continue
			}
			host, derived = h, h
		}
		name := ing.Name
		if len(groups) > 1 {
			name = fmt.Sprintf("%s-%d", ing.Name, i)
		}
		rt := &gwv1.HTTPRoute{
			ObjectMeta: src.meta(name),
			Spec: gwv1.HTTPRouteSpec{
				Hostnames: []gwv1.Hostname{gwv1.Hostname(host)},
				Rules:     g.rules,
			},
		}
		if derived != "" {
			rt.Annotations[config.AnnotationDefaultHostname] = derived
			if !r.hadDerived(ctx, ing.Namespace, name, derived) {
				r.event(ing, corev1.EventTypeNormal, "DefaultHostname", "rules without a host are published as %s", derived)
			}
		}
		routes = append(routes, genRoute{obj: rt})
	}
	return routes, nil
}

// hadDerived reports whether the existing route already carries derived.
func (g *generator) hadDerived(ctx context.Context, ns, name, derived string) bool {
	rt := &gwv1.HTTPRoute{}
	if err := g.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, rt); err != nil {
		return false
	}
	return rt.Annotations[config.AnnotationDefaultHostname] == derived
}

// hasRootPrefix reports whether a rule already matches every path.
func hasRootPrefix(rules []gwv1.HTTPRouteRule) bool {
	for _, rule := range rules {
		for _, m := range rule.Matches {
			if m.Path != nil && m.Path.Type != nil && *m.Path.Type == gwv1.PathMatchPathPrefix &&
				m.Path.Value != nil && *m.Path.Value == "/" {
				return true
			}
		}
	}
	return false
}

// backendRef converts an Ingress backend, resolving named ports through
// the Service.
func (r *IngressReconciler) backendRef(ctx context.Context, ing *networkingv1.Ingress, b networkingv1.IngressBackend) (gwv1.BackendRef, bool) {
	if b.Service == nil {
		r.warn(ing, "UnsupportedBackend", "only Service backends are supported")
		return gwv1.BackendRef{}, false
	}
	port := b.Service.Port.Number
	if port == 0 && b.Service.Port.Name != "" {
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ing.Namespace, Name: b.Service.Name}, svc); err != nil {
			r.warn(ing, "BackendNotFound", "Service %q for named port %q: %v", b.Service.Name, b.Service.Port.Name, err)
			return gwv1.BackendRef{}, false
		}
		for _, sp := range svc.Spec.Ports {
			if sp.Name == b.Service.Port.Name {
				port = sp.Port
			}
		}
		if port == 0 {
			r.warn(ing, "BackendNotFound", "Service %q has no port named %q", b.Service.Name, b.Service.Port.Name)
			return gwv1.BackendRef{}, false
		}
	}
	if port == 0 {
		port = 80
	}
	return gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
		Name: gwv1.ObjectName(b.Service.Name), Port: ptr.To(gwv1.PortNumber(port)),
	}}, true
}

// ingressesInNamespace enqueues Ingresses in the object's namespace.
func (r *IngressReconciler) ingressesInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetNamespace()
	if _, isNS := obj.(*corev1.Namespace); isNS {
		ns = obj.GetName()
	}
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ing := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}})
	}
	return out
}

func (r *IngressReconciler) allIngresses(ctx context.Context, _ client.Object) []reconcile.Request {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ing := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ing.Namespace, Name: ing.Name}})
	}
	return out
}

// SetupWithManager registers the reconciler.
func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		Named("ingress").
		For(&networkingv1.Ingress{}).
		Owns(&gwv1.Gateway{}).
		Owns(&gwv1.HTTPRoute{}).
		Watches(&networkingv1.IngressClass{}, handler.EnqueueRequestsFromMapFunc(r.allIngresses)).
		Watches(&gwv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(r.allIngresses)).
		Watches(&gwv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.allIngresses), builder.WithPredicates(gatewayStatusChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.ingressesInNamespace), builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.ingressesInNamespace),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
