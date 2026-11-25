package controller

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// managedGateways returns every Gateway of a class this controller serves,
// and every Gateway still carrying its finalizer.
func (r *GatewayReconciler) managedGateways(ctx context.Context) []reconcile.Request {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	classes := map[string]bool{}
	var out []reconcile.Request
	for _, gw := range list.Items {
		name := string(gw.Spec.GatewayClassName)
		managed, seen := classes[name]
		if !seen {
			gc, _ := r.managedClass(ctx, name)
			managed = gc != nil
			classes[name] = managed
		}
		if managed || controllerutil.ContainsFinalizer(&gw, r.Config.Finalizer) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (r *GatewayReconciler) allGateways(ctx context.Context, _ client.Object) []reconcile.Request {
	return r.managedGateways(ctx)
}

func (r *GatewayReconciler) gatewaysForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, gw := range list.Items {
		if string(gw.Spec.GatewayClassName) == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}})
		}
	}
	return out
}

// gatewaysForSecret maps a class credentials Secret to the Gateways of
// the classes that use it.
func (r *GatewayReconciler) gatewaysForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var out []reconcile.Request
	for _, req := range classesForSecret(r.Client, r.Config.ControllerName)(ctx, obj) {
		gc := &gwv1.GatewayClass{}
		gc.Name = req.Name
		out = append(out, r.gatewaysForClass(ctx, gc)...)
	}
	return out
}

// gatewaysForConfigMap maps an infrastructure parameters ConfigMap to the
// Gateways in its namespace that reference it.
func (r *GatewayReconciler) gatewaysForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, gw := range list.Items {
		if inf := gw.Spec.Infrastructure; inf != nil && inf.ParametersRef != nil && string(inf.ParametersRef.Kind) == "ConfigMap" && inf.ParametersRef.Name == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}})
		}
	}
	return out
}

// gatewaysForRoute maps a route to the Gateways in its parentRefs and in
// its status (so that detaching updates the old Gateway). A parentRef to a
// ListenerSet maps to the ListenerSet's Gateway.
func (r *GatewayReconciler) gatewaysForRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	var parents []gwv1.ParentReference
	var status *gwv1.RouteStatus
	switch rt := obj.(type) {
	case *gwv1.HTTPRoute:
		parents, status = rt.Spec.ParentRefs, &rt.Status.RouteStatus
	case *gwv1.GRPCRoute:
		parents, status = rt.Spec.ParentRefs, &rt.Status.RouteStatus
	case *gwv1.TCPRoute:
		parents, status = rt.Spec.ParentRefs, &rt.Status.RouteStatus
	default:
		return nil
	}
	for _, p := range status.Parents {
		if string(p.ControllerName) == r.Config.ControllerName {
			parents = append(parents, p.ParentRef)
		}
	}
	seen := map[types.NamespacedName]bool{}
	var out []reconcile.Request
	for _, p := range parents {
		if key, ok := listenerSetParentRef(p, obj.GetNamespace()); ok {
			// A ListenerSet that is gone leaves stale status, which any
			// Gateway's reconcile removes.
			if !r.Features.ListenerSet() {
				continue
			}
			ls := &gwv1.ListenerSet{}
			var reqs []reconcile.Request
			if err := r.Get(ctx, key, ls); err == nil {
				reqs = r.gatewayForListenerSet(ctx, ls)
			} else if apierrors.IsNotFound(err) {
				if all := r.managedGateways(ctx); len(all) > 0 {
					reqs = all[:1]
				}
			}
			for _, req := range reqs {
				if !seen[req.NamespacedName] {
					seen[req.NamespacedName] = true
					out = append(out, req)
				}
			}
			continue
		}
		if p.Group != nil && string(*p.Group) != gwv1.GroupName || p.Kind != nil && string(*p.Kind) != translate.KindGateway {
			continue
		}
		ns := obj.GetNamespace()
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		key := types.NamespacedName{Namespace: ns, Name: string(p.Name)}
		if !seen[key] {
			seen[key] = true
			out = append(out, reconcile.Request{NamespacedName: key})
		}
	}
	return out
}

// hostnamesChanged passes Gateway updates that change the hostnames
// another Gateway publishes, which may free or take a hostname.
var hostnamesChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		return e.ObjectOld.GetAnnotations()[config.AnnotationHostnames] != e.ObjectNew.GetAnnotations()[config.AnnotationHostnames]
	},
	DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetAnnotations()[config.AnnotationHostnames] != "" },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// servicePortsChanged passes Service creation, deletion, and changes of
// the ports, which route translation reads (port numbers, names and
// appProtocol).
var servicePortsChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	DeleteFunc: func(event.DeleteEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*corev1.Service)
		n, ok2 := e.ObjectNew.(*corev1.Service)
		return !ok1 || !ok2 || !equality.Semantic.DeepEqual(o.Spec.Ports, n.Spec.Ports)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// namespaceChanged passes namespace label and annotation changes (route
// selectors and policy).
var namespaceChanged = predicate.Or(predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{})

// SetupWithManager registers the reconciler and its watches. Watches for
// optional route kinds are added when they appear (see FeatureWatcher).
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager, fw *FeatureWatcher) error {
	r.APIReader = mgr.GetAPIReader()
	routeHandler := handler.EnqueueRequestsFromMapFunc(r.gatewaysForRoute)
	all := handler.EnqueueRequestsFromMapFunc(r.allGateways)
	b := ctrl.NewControllerManagedBy(mgr).
		Named("gateway").
		For(&gwv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Watches(&gwv1.Gateway{}, all, builder.WithPredicates(hostnamesChanged)).
		Watches(&gwv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(r.gatewaysForClass)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.gatewaysForConfigMap)).
		WatchesMetadata(secretMetadata(), handler.EnqueueRequestsFromMapFunc(r.gatewaysForSecret),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&corev1.Service{}, all, builder.WithPredicates(servicePortsChanged)).
		Watches(&corev1.Namespace{}, all, builder.WithPredicates(namespaceChanged)).
		Watches(&gwv1.HTTPRoute{}, routeHandler)
	if r.Features.GRPCRoute() {
		b = b.Watches(&gwv1.GRPCRoute{}, routeHandler)
	}
	if r.Features.TCPRoute() {
		b = b.Watches(&gwv1.TCPRoute{}, routeHandler)
	}
	if r.Features.ReferenceGrant() {
		b = b.Watches(&gwv1.ReferenceGrant{}, all)
	}
	// Old and new parents are both enqueued on update. Status writes do
	// not change the generation and are skipped.
	setHandler := handler.EnqueueRequestsFromMapFunc(r.gatewayForListenerSet)
	setPredicate := predicate.GenerationChangedPredicate{}
	if r.Features.ListenerSet() {
		b = b.Watches(&gwv1.ListenerSet{}, setHandler, builder.WithPredicates(setPredicate))
	}
	c, err := b.Build(r)
	if err != nil {
		return err
	}
	if err := mgr.Add(manager.RunnableFunc(r.sweepRecords)); err != nil {
		return err
	}
	if fw == nil {
		return nil
	}
	watch := func(obj client.Object, h handler.EventHandler) func() error {
		return func() error {
			// The new informer's initial list delivers create events for
			// existing objects, so their Gateways are reconciled.
			return c.Watch(source.Kind(mgr.GetCache(), obj, h))
		}
	}
	if !r.Features.GRPCRoute() {
		fw.OnGRPCRoute(watch(&gwv1.GRPCRoute{}, routeHandler))
	}
	if !r.Features.TCPRoute() {
		fw.OnTCPRoute(watch(&gwv1.TCPRoute{}, routeHandler))
	}
	if !r.Features.ReferenceGrant() {
		fw.OnReferenceGrant(watch(&gwv1.ReferenceGrant{}, all))
	}
	if !r.Features.ListenerSet() {
		fw.OnListenerSet(func() error {
			return c.Watch(source.Kind(mgr.GetCache(), client.Object(&gwv1.ListenerSet{}), setHandler, setPredicate))
		})
	}
	return nil
}
