package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// MaxListeners is the Gateway API limit on listeners per Gateway.
const MaxListeners = 64

// SharedGatewayReconciler creates, updates and deletes the shared Gateways
// that generated routes attach to.
type SharedGatewayReconciler struct {
	generator
}

// NewSharedGatewayReconciler returns a reconciler.
func NewSharedGatewayReconciler(c client.Client, scheme *runtime.Scheme, cfg *config.Config, cf *cloudflare.Client, f *Features, rec events.EventRecorder) *SharedGatewayReconciler {
	return &SharedGatewayReconciler{generator: generator{classResolver: classResolver{Client: c, Config: cfg, CF: cf}, Scheme: scheme, Features: f, Recorder: rec}}
}

// configured reports whether key is a shared Gateway the configuration
// asks for (as opposed to one left over from an earlier configuration).
func (r *SharedGatewayReconciler) configured(key types.NamespacedName) bool {
	if key.Name != r.Config.SharedName {
		return false
	}
	return r.Config.SharedScope == config.ScopeNamespace || key.Namespace == r.Config.SharedNamespace
}

// users returns the generated routes that use or want the shared Gateway.
func (r *SharedGatewayReconciler) users(ctx context.Context, key types.NamespacedName) ([]*gwv1.HTTPRoute, []*gwv1.TCPRoute, error) {
	uses := func(o client.Object, refs []gwv1.ParentReference) bool {
		if o.GetAnnotations()[config.AnnotationGateway] == key.String() {
			return true
		}
		for _, p := range refs {
			if translate.ParentTargets(p, o.GetNamespace(), key.Namespace, key.Name) {
				return true
			}
		}
		return false
	}
	opts := []client.ListOption{client.HasLabels{config.LabelSourceKind}, client.MatchingLabels(managedByLabels(r.Config))}
	if r.Config.SharedScope == config.ScopeNamespace && r.configured(key) {
		opts = append(opts, client.InNamespace(key.Namespace))
	}
	var hl gwv1.HTTPRouteList
	if err := r.List(ctx, &hl, opts...); err != nil {
		return nil, nil, err
	}
	var http []*gwv1.HTTPRoute
	for i := range hl.Items {
		if uses(&hl.Items[i], hl.Items[i].Spec.ParentRefs) {
			http = append(http, &hl.Items[i])
		}
	}
	var tcp []*gwv1.TCPRoute
	if r.Features.TCPRoute() {
		var tl gwv1.TCPRouteList
		if err := r.List(ctx, &tl, opts...); err != nil {
			return nil, nil, err
		}
		for i := range tl.Items {
			if uses(&tl.Items[i], tl.Items[i].Spec.ParentRefs) {
				tcp = append(tcp, &tl.Items[i])
			}
		}
	}
	return http, tcp, nil
}

// isShared reports whether gw is a shared Gateway of this installation.
func isShared(cfg *config.Config, gw *gwv1.Gateway) bool {
	return gw.Annotations[config.AnnotationManaged] == config.ManagedShared &&
		gw.Labels[connector.LabelManagedBy] == cfg.Instance
}

// Reconcile implements reconcile.Reconciler.
func (r *SharedGatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	key := req.NamespacedName
	existing := &gwv1.Gateway{}
	exists := true
	if err := r.Get(ctx, key, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		exists = false
	}
	if exists && !isShared(r.Config, existing) {
		return ctrl.Result{}, nil // never touch a Gateway we did not create
	}
	http, tcp, err := r.users(ctx, key)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(http) == 0 && len(tcp) == 0 {
		if exists && existing.DeletionTimestamp.IsZero() && (r.Config.SharedScope == config.ScopeNamespace || !r.configured(key)) {
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, existing))
		}
		return ctrl.Result{}, nil
	}
	if exists && !existing.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil // recreate once it is gone
	}
	gw, err := r.desired(ctx, key, http, tcp)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, apply(ctx, r.Client, r.Scheme, gw)
}

// desired builds the shared Gateway.
func (r *SharedGatewayReconciler) desired(ctx context.Context, key types.NamespacedName, http []*gwv1.HTTPRoute, tcp []*gwv1.TCPRoute) (*gwv1.Gateway, error) {
	pol, err := namespacePolicy(ctx, r.Client, r.Config, key.Namespace)
	if err != nil {
		return nil, err
	}
	gw := &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace, Name: key.Name,
			Annotations: map[string]string{config.AnnotationManaged: config.ManagedShared},
			Labels:      managedByLabels(r.Config),
		},
		Spec: gwv1.GatewaySpec{GatewayClassName: gwv1.ObjectName(generatedClass(r.Config, pol))},
	}
	from := gwv1.NamespacesFromSame
	if r.Config.SharedScope == config.ScopeCluster {
		from = gwv1.NamespacesFromAll
	}
	httpListener := func(name string, host string) gwv1.Listener {
		l := gwv1.Listener{Name: gwv1.SectionName(name), Protocol: gwv1.HTTPProtocolType, Port: 80,
			AllowedRoutes: &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{From: ptr.To(from)}}}
		if host != "" {
			l.Hostname = ptr.To(gwv1.Hostname(host))
		}
		return l
	}
	if len(http) > 0 || len(tcp) == 0 {
		if r.Config.SharedScope == config.ScopeNamespace && pol.AllowedHostnames != nil {
			// Listener hostnames repeat the namespace's allowlist; derived
			// hostnames, which are exempt from it, get their own listener.
			hosts := append([]string(nil), pol.AllowedHostnames...)
			base := pol.BaseDomain
			if base == "" {
				base = r.Config.BaseDomain
			}
			seen := map[string]bool{}
			for i, h := range hosts {
				if seen[h] || len(gw.Spec.Listeners) >= MaxListeners-1 {
					continue
				}
				seen[h] = true
				name := "http"
				if i > 0 {
					name = fmt.Sprintf("http-%d", i)
				}
				gw.Spec.Listeners = append(gw.Spec.Listeners, httpListener(name, h))
			}
			if base != "" && !seen["*."+base] {
				gw.Spec.Listeners = append(gw.Spec.Listeners, httpListener("http-default", "*."+base))
			}
		}
		if len(gw.Spec.Listeners) == 0 {
			gw.Spec.Listeners = append(gw.Spec.Listeners, httpListener("http", ""))
		}
	}
	var hosts []tcpListener
	for _, rt := range tcp {
		hosts = append(hosts, tcpListener{Host: rt.Annotations[config.AnnotationHostname], Namespace: rt.Namespace})
	}
	gw.Spec.Listeners = append(gw.Spec.Listeners, tcpListeners(hosts, from)...)
	if p := r.Config.SharedParameters; p != "" {
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: p}, cm); err == nil {
			gw.Spec.Infrastructure = &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{Group: "", Kind: "ConfigMap", Name: p}}
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return gw, nil
}

// ---- watches ----

// sharedKeysForRoute maps a generated route to the shared Gateways it uses
// or wants.
func (r *SharedGatewayReconciler) sharedKeysForRoute(_ context.Context, obj client.Object) []reconcile.Request {
	if l := obj.GetLabels(); l[config.LabelSourceKind] == "" || l[connector.LabelManagedBy] != r.Config.Instance {
		return nil
	}
	keys := map[string]bool{}
	if v := obj.GetAnnotations()[config.AnnotationGateway]; v != "" {
		keys[v] = true
	}
	var refs []gwv1.ParentReference
	switch rt := obj.(type) {
	case *gwv1.HTTPRoute:
		refs = rt.Spec.ParentRefs
	case *gwv1.TCPRoute:
		refs = rt.Spec.ParentRefs
	}
	for _, p := range refs {
		ns := obj.GetNamespace()
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		keys[ns+"/"+string(p.Name)] = true
	}
	var out []reconcile.Request
	for k := range keys {
		ns, name, _ := strings.Cut(k, "/")
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	}
	return out
}

func (r *SharedGatewayReconciler) sharedKeyForNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	ns := obj.GetNamespace()
	if _, isNS := obj.(*corev1.Namespace); isNS {
		ns = obj.GetName()
	}
	if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name != r.Config.SharedParameters {
		return nil
	}
	return []reconcile.Request{{NamespacedName: SharedKey(r.Config, ns)}}
}

// isSharedGateway passes Gateways the controller manages as shared, and
// Gateways with a shared name (so that deleting a user's Gateway in the
// way lets the shared one be created).
func (r *SharedGatewayReconciler) isSharedGateway() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetAnnotations()[config.AnnotationManaged] == config.ManagedShared || o.GetName() == r.Config.SharedName
	})
}

// gatewayStatusChanged passes Gateway updates that change whether it is
// programmed or its addresses, which sources report.
var gatewayStatusChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*gwv1.Gateway)
		n, ok2 := e.ObjectNew.(*gwv1.Gateway)
		if !ok1 || !ok2 {
			return false
		}
		oc := meta.FindStatusCondition(o.Status.Conditions, string(gwv1.GatewayConditionProgrammed))
		nc := meta.FindStatusCondition(n.Status.Conditions, string(gwv1.GatewayConditionProgrammed))
		if (oc == nil) != (nc == nil) || oc != nil && (oc.Status != nc.Status || oc.ObservedGeneration != nc.ObservedGeneration) {
			return true
		}
		if len(o.Status.Addresses) != len(n.Status.Addresses) {
			return true
		}
		for i := range o.Status.Addresses {
			if o.Status.Addresses[i].Value != n.Status.Addresses[i].Value {
				return true
			}
		}
		return false
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// SetupWithManager registers the reconciler.
func (r *SharedGatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	routes := handler.EnqueueRequestsFromMapFunc(r.sharedKeysForRoute)
	b := ctrl.NewControllerManagedBy(mgr).
		Named("shared-gateway").
		For(&gwv1.Gateway{}, builder.WithPredicates(r.isSharedGateway())).
		Watches(&gwv1.HTTPRoute{}, routes).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.sharedKeyForNamespace), builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.sharedKeyForNamespace))
	if r.Features.TCPRoute() {
		b = b.Watches(&gwv1.TCPRoute{}, routes)
	}
	return b.Complete(r)
}
