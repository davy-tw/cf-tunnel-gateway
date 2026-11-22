package controller

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// ServiceReconciler translates LoadBalancer Services of the configured
// loadBalancerClass into routes on a shared or dedicated Gateway.
type ServiceReconciler struct {
	generator
}

// NewServiceReconciler returns a reconciler.
func NewServiceReconciler(c client.Client, scheme *runtime.Scheme, cfg *config.Config, cf *cloudflare.Client, f *Features, rec events.EventRecorder) *ServiceReconciler {
	return &ServiceReconciler{generator: generator{classResolver: classResolver{Client: c, Config: cfg, CF: cf}, Scheme: scheme, Features: f, Recorder: rec}}
}

func serviceSource(svc *corev1.Service) sourceObj {
	return sourceObj{obj: svc, kind: "Service", apiVersion: "v1", label: config.SourceService}
}

func (r *ServiceReconciler) managed(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer && svc.Spec.LoadBalancerClass != nil &&
		*svc.Spec.LoadBalancerClass == r.Config.LoadBalancerClass
}

// Reconcile implements reconcile.Reconciler.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	src := serviceSource(svc)
	if !svc.DeletionTimestamp.IsZero() || !r.managed(svc) {
		if !controllerutil.ContainsFinalizer(svc, r.Config.Finalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanupSource(ctx, src); err != nil {
			return ctrl.Result{}, err
		}
		if svc.DeletionTimestamp.IsZero() {
			if err := r.writeStatus(ctx, svc, nil); err != nil {
				return ctrl.Result{}, err
			}
		}
		orig := svc.DeepCopy()
		controllerutil.RemoveFinalizer(svc, r.Config.Finalizer)
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, svc, client.MergeFrom(orig)))
	}
	if !controllerutil.ContainsFinalizer(svc, r.Config.Finalizer) {
		orig := svc.DeepCopy()
		controllerutil.AddFinalizer(svc, r.Config.Finalizer)
		if err := r.Patch(ctx, svc, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}

	plan, err := r.plan(ctx, svc)
	if err != nil {
		return ctrl.Result{}, ignoreRateLimit(err)
	}
	for _, w := range plan.Warnings {
		r.warn(svc, "PortNotPublished", "%s", w)
	}
	routes, tcpHosts := r.objects(svc, plan)
	a, err := r.syncSource(ctx, src, routes, tcpHosts)
	if err != nil {
		return ctrl.Result{}, err
	}
	if a == nil {
		return ctrl.Result{}, r.writeStatus(ctx, svc, nil)
	}
	if a.serving == "" {
		return ctrl.Result{RequeueAfter: a.requeue}, nil
	}
	var lbs []corev1.LoadBalancerIngress
	if r.sourceStatusTarget(ctx, a) != "" {
		web := []corev1.PortStatus{{Port: 443, Protocol: corev1.ProtocolTCP}, {Port: 80, Protocol: corev1.ProtocolTCP}}
		for _, h := range plan.Hostnames {
			lbs = append(lbs, corev1.LoadBalancerIngress{Hostname: h, Ports: web})
		}
		for _, t := range plan.TCP {
			lbs = append(lbs, corev1.LoadBalancerIngress{Hostname: t.Host,
				Ports: []corev1.PortStatus{{Port: t.Port, Protocol: corev1.ProtocolTCP}}})
		}
	}
	return ctrl.Result{RequeueAfter: a.requeue}, r.writeStatus(ctx, svc, lbs)
}

func (r *ServiceReconciler) writeStatus(ctx context.Context, svc *corev1.Service, lbs []corev1.LoadBalancerIngress) error {
	if reflect.DeepEqual(lbs, svc.Status.LoadBalancer.Ingress) || len(lbs) == 0 && len(svc.Status.LoadBalancer.Ingress) == 0 {
		return nil
	}
	orig := svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = lbs
	return r.Status().Patch(ctx, svc, client.MergeFrom(orig))
}

// ServicePlan is how a LoadBalancer Service is published.
type ServicePlan struct {
	// HTTP is the port served at Hostnames, if any.
	HTTP      *corev1.ServicePort
	Hostnames []string
	// Derived lists the hostnames the controller derived.
	Derived  []string
	TCP      []tcpListener
	Warnings []string
}

// httpAppProtocols mark a port as the HTTP port.
var httpAppProtocols = map[string]bool{
	"http": true, "kubernetes.io/h2c": true, "kubernetes.io/ws": true, "https": true, "kubernetes.io/wss": true,
}

// ParseTCPHostnames parses "<port>[=<hostname>][,...]". A port without a
// hostname maps to "".
func ParseTCPHostnames(v string) (map[int32]string, error) {
	out := map[int32]string{}
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		p, h, _ := strings.Cut(e, "=")
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 32)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%q: not a port", e)
		}
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			if errs := validation.IsDNS1123Subdomain(h); len(errs) > 0 || strings.HasPrefix(h, "*") {
				return nil, fmt.Errorf("%q: not a hostname", e)
			}
		}
		out[int32(n)] = h
	}
	return out, nil
}

// plan decides which port is served over HTTP and which over TCP.
func (r *ServiceReconciler) plan(ctx context.Context, svc *corev1.Service) (ServicePlan, error) {
	src := serviceSource(svc)
	var plan ServicePlan
	tcp := map[int32]string{}
	if v := svc.Annotations[config.AnnotationTCPHostnames]; v != "" {
		switch {
		case !r.Config.TCPMode:
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("annotation %s is ignored: TCP mode is off", config.AnnotationTCPHostnames))
		case !r.Features.TCPRoute():
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("annotation %s is ignored: the TCPRoute CRD is not installed", config.AnnotationTCPHostnames))
		default:
			var err error
			if tcp, err = ParseTCPHostnames(v); err != nil {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("annotation %s: %v", config.AnnotationTCPHostnames, err))
				tcp = map[int32]string{}
			}
		}
	}
	var candidates []corev1.ServicePort
	for _, p := range svc.Spec.Ports {
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("port %d/%s is not published: tunnels carry TCP only", p.Port, p.Protocol))
			continue
		}
		if _, isTCP := tcp[p.Port]; !isTCP {
			candidates = append(candidates, p)
		}
	}
	for i := range candidates {
		if httpAppProtocols[ptr.Deref(candidates[i].AppProtocol, "")] {
			plan.HTTP = &candidates[i]
			break
		}
	}
	if plan.HTTP == nil && len(candidates) > 0 {
		plan.HTTP = &candidates[0]
	}
	if plan.HTTP != nil {
		for _, h := range strings.Split(svc.Annotations[config.AnnotationHostname], ",") {
			if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
				plan.Hostnames = append(plan.Hostnames, h)
			}
		}
		if len(plan.Hostnames) == 0 {
			h, why, err := r.derivedHostname(ctx, src, "")
			if err != nil {
				return plan, err
			}
			if h == "" {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("port %d is not published: a tunnel publishes by hostname only; set annotation %s (%s)", plan.HTTP.Port, config.AnnotationHostname, why))
				plan.HTTP = nil
			} else {
				plan.Hostnames, plan.Derived = []string{h}, []string{h}
			}
		}
		for _, p := range candidates {
			if plan.HTTP != nil && p.Port != plan.HTTP.Port {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("port %d is not published: only one port is served over HTTP (list it in %s to publish it over TCP)", p.Port, config.AnnotationTCPHostnames))
			}
		}
	}
	ports := map[int32]bool{}
	for _, p := range svc.Spec.Ports {
		ports[p.Port] = true
	}
	for _, port := range sortedPorts(tcp) {
		host := tcp[port]
		if !ports[port] {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("annotation %s: the Service has no port %d", config.AnnotationTCPHostnames, port))
			continue
		}
		if host == "" {
			h, why, err := r.derivedHostname(ctx, src, strconv.Itoa(int(port)))
			if err != nil {
				return plan, err
			}
			if h == "" {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("TCP port %d is not published: it has no hostname (%s)", port, why))
				continue
			}
			host = h
			plan.Derived = append(plan.Derived, h)
		}
		plan.TCP = append(plan.TCP, tcpListener{Host: host, Port: port, Namespace: svc.Namespace})
	}
	return plan, nil
}

func sortedPorts(m map[int32]string) []int32 {
	var out []int32
	for p := range m {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (r *ServiceReconciler) objects(svc *corev1.Service, plan ServicePlan) ([]genRoute, []tcpListener) {
	src := serviceSource(svc)
	derived := map[string]bool{}
	for _, h := range plan.Derived {
		derived[h] = true
	}
	var routes []genRoute
	if p := plan.HTTP; p != nil {
		rt := &gwv1.HTTPRoute{
			ObjectMeta: src.meta(r.Config, fmt.Sprintf("%s-http-%d", svc.Name, p.Port)),
			Spec: gwv1.HTTPRouteSpec{
				Rules: []gwv1.HTTPRouteRule{{
					Matches: []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Type: ptr.To(gwv1.PathMatchPathPrefix), Value: ptr.To("/")}}},
					BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
						Name: gwv1.ObjectName(svc.Name), Port: ptr.To(gwv1.PortNumber(p.Port))}}}},
				}},
			},
		}
		var d []string
		for _, h := range plan.Hostnames {
			rt.Spec.Hostnames = append(rt.Spec.Hostnames, gwv1.Hostname(h))
			if derived[h] {
				d = append(d, h)
			}
		}
		if len(d) > 0 {
			rt.Annotations[config.AnnotationDefaultHostname] = strings.Join(d, ",")
		}
		routes = append(routes, genRoute{obj: rt})
	}
	for _, t := range plan.TCP {
		rt := &gwv1.TCPRoute{
			ObjectMeta: src.meta(r.Config, fmt.Sprintf("%s-tcp-%d", svc.Name, t.Port)),
			Spec: gwv1.TCPRouteSpec{
				Rules: []gwv1.TCPRouteRule{{BackendRefs: []gwv1.BackendRef{{BackendObjectReference: gwv1.BackendObjectReference{
					Name: gwv1.ObjectName(svc.Name), Port: ptr.To(gwv1.PortNumber(t.Port)),
				}}}}},
			},
		}
		rt.Annotations[config.AnnotationHostname] = t.Host
		if derived[t.Host] {
			rt.Annotations[config.AnnotationDefaultHostname] = t.Host
		}
		routes = append(routes, genRoute{obj: rt, tcpHost: t.Host})
	}
	return routes, plan.TCP
}

// managedServices enqueues every Service of the LoadBalancer class.
func (r *ServiceReconciler) managedServices(ctx context.Context, _ client.Object) []reconcile.Request {
	var list corev1.ServiceList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if r.managed(&list.Items[i]) || controllerutil.ContainsFinalizer(&list.Items[i], r.Config.Finalizer) {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

// SetupWithManager registers the reconciler. TCPRoutes generated later
// (after the CRD appears) are picked up through the Gateway watch.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	b := ctrl.NewControllerManagedBy(mgr).
		Named("service").
		For(&corev1.Service{}).
		Owns(&gwv1.Gateway{}).
		Owns(&gwv1.HTTPRoute{}).
		Watches(&gwv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(r.managedServices)).
		Watches(&gwv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.managedServices), builder.WithPredicates(gatewayStatusChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.managedServices), builder.WithPredicates(predicate.AnnotationChangedPredicate{}))
	if r.Features.TCPRoute() {
		b = b.Owns(&gwv1.TCPRoute{})
	}
	return b.Complete(r)
}
