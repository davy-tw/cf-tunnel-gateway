package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// Requeue intervals.
const (
	pendingRequeue = 5 * time.Second
	// refreshEvery rewrites the tunnel configuration and rechecks DNS and
	// connections even without changes, repairing drift.
	refreshEvery = 10 * time.Minute
)

// Reasons.
const (
	ReasonNotAllowed = "NotAllowed"
)

// GatewayReconciler provisions tunnels, connectors and DNS for managed
// Gateways and writes Gateway and route status.
type GatewayReconciler struct {
	classResolver
	Scheme   *runtime.Scheme
	Features *Features
	Recorder events.EventRecorder
	// ClusterID identifies the cluster in tunnel names (the kube-system
	// namespace UID).
	ClusterID string

	mu      sync.Mutex
	configs map[string]configEntry // tunnel ID -> last written configuration
	tokens  map[string]tokenEntry  // tunnel ID -> connector token
}

type configEntry struct {
	data []byte
	at   time.Time
}

// NewGatewayReconciler returns a reconciler.
func NewGatewayReconciler(c client.Client, scheme *runtime.Scheme, cfg *config.Config, cf *cloudflare.Client, f *Features, rec events.EventRecorder, clusterID string) *GatewayReconciler {
	return &GatewayReconciler{
		classResolver: classResolver{Client: c, Config: cfg, CF: cf},
		Scheme:        scheme, Features: f, Recorder: rec, ClusterID: clusterID,
		configs: map[string]configEntry{}, tokens: map[string]tokenEntry{},
	}
}

// pendingError is a provisioning step that cannot finish yet; it is
// reported on Programmed without being treated as a failure.
type pendingError struct{ msg string }

func (e *pendingError) Error() string { return e.msg }

func (r *GatewayReconciler) event(obj runtime.Object, typ, reason, msg string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, typ, reason, "Reconcile", msg, args...)
	}
}

// outcome collects what provisioning achieved, for status.
type outcome struct {
	tunnel     *cloudflare.Tunnel
	ready      int32
	desired    int32
	disabled   bool
	dnsManaged bool
	dns        map[string]dnsState // nil until DNS was synced
	dnsErr     error
}

// Reconcile implements reconcile.Reconciler.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gw := &gwv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	res, err := r.reconcile(ctx, gw)
	if d, ok := cloudflare.RetryAfter(err); ok {
		log.FromContext(ctx).Info("Cloudflare API rate limit; backing off", "after", d)
		return ctrl.Result{RequeueAfter: d}, nil
	}
	return res, err
}

func (r *GatewayReconciler) reconcile(ctx context.Context, gw *gwv1.Gateway) (ctrl.Result, error) {
	gc, classExists, err := r.lookupClass(ctx, string(gw.Spec.GatewayClassName))
	if err != nil {
		return ctrl.Result{}, err
	}
	deleting := !gw.DeletionTimestamp.IsZero()
	if gc == nil || deleting {
		if !controllerutil.ContainsFinalizer(gw, r.Config.Finalizer) {
			return ctrl.Result{}, nil
		}
		if !deleting && !classExists {
			// The class is gone, perhaps only for a moment. Keep the tunnel
			// and connectors running and report it.
			return ctrl.Result{}, r.writeClassMissingStatus(ctx, gw)
		}
		done, err := r.finalize(ctx, gw, !deleting)
		if err != nil {
			if d, ok := cloudflare.RetryAfter(err); ok {
				return ctrl.Result{RequeueAfter: d}, nil
			}
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		if err := r.deleteRecord(ctx, gw); err != nil {
			return ctrl.Result{}, err
		}
		orig := gw.DeepCopy()
		controllerutil.RemoveFinalizer(gw, r.Config.Finalizer)
		for _, a := range controllerAnnotations {
			delete(gw.Annotations, a)
		}
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, gw, client.MergeFrom(orig)))
	}
	if !controllerutil.ContainsFinalizer(gw, r.Config.Finalizer) {
		orig := gw.DeepCopy()
		controllerutil.AddFinalizer(gw, r.Config.Finalizer)
		if err := r.Patch(ctx, gw, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}

	rs, err := r.listRoutes(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Namespace policy.
	pol, err := namespacePolicy(ctx, r.Client, r.Config, gw.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pol.classAllowed(gc.Name) {
		msg := fmt.Sprintf("namespace %s does not allow GatewayClass %q (annotation %s)", gw.Namespace, gc.Name, config.AnnotationAllowedGatewayClasses)
		res := &translate.Result{AcceptedReason: ReasonNotAllowed, AcceptedMessage: msg}
		relErr := r.release(ctx, gw)
		var blocked *pendingError
		if errors.As(relErr, &blocked) {
			relErr = nil
		}
		if err := errors.Join(
			r.withdraw(ctx, gw),
			relErr,
			r.writeRouteStatus(ctx, gw, rs, res, nil),
			r.writeGatewayStatus(ctx, gw, res, nil, &pendingError{msg}, &outcome{}),
		); err != nil {
			return ctrl.Result{}, err
		}
		if blocked != nil {
			return ctrl.Result{RequeueAfter: pendingRequeue}, nil
		}
		return ctrl.Result{}, nil
	}

	creds, cerr := r.gatewayCredentials(ctx, gw)
	var ip *invalidParamsError
	if cerr != nil && !asInvalid(cerr, &ip) {
		return ctrl.Result{}, cerr
	}
	if ip != nil {
		ip = &invalidParamsError{"GatewayClass is not usable: " + ip.msg}
	}
	params, perr := r.infraParams(ctx, gw)
	if perr != nil && ip == nil && !asInvalid(perr, &ip) {
		return ctrl.Result{}, perr
	}

	in, err := r.translateInput(ctx, gw, rs)
	if err != nil {
		return ctrl.Result{}, err
	}
	res := translate.Translate(in)

	out := &outcome{disabled: params.Disabled, dnsManaged: gw.Annotations[config.AnnotationDNS] != "disabled"}
	var progErr error
	switch {
	case ip != nil:
		// Invalid parameters keep the last good connector running.
		progErr = &pendingError{ip.msg}
	case !res.Accepted:
		progErr = &pendingError{"Gateway is not accepted: " + res.AcceptedMessage}
		if err := r.withdraw(ctx, gw); err != nil {
			return ctrl.Result{}, err
		}
	default:
		progErr = r.provision(ctx, gw, creds, params, res, out)
	}
	if _, limited := cloudflare.RetryAfter(progErr); limited {
		return ctrl.Result{}, progErr
	}
	var pe *pendingError
	var tunnelIP *invalidParamsError
	switch {
	case progErr == nil, errors.As(progErr, &pe):
	case asInvalid(progErr, &tunnelIP):
		ip = tunnelIP
	default:
		log.FromContext(ctx).Error(progErr, "provisioning failed")
		r.event(gw, corev1.EventTypeWarning, "ProvisioningFailed", "%v", progErr)
	}

	statusErr := errors.Join(
		r.writeRouteStatus(ctx, gw, rs, res, out),
		r.writeGatewayStatus(ctx, gw, res, ip, progErr, out),
	)
	if statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	switch {
	case ip != nil || !res.Accepted:
		return ctrl.Result{RequeueAfter: refreshEvery}, nil
	case progErr != nil && pe == nil:
		return ctrl.Result{}, progErr
	case !r.programmed(out) || pe != nil || out.dnsErr != nil:
		return ctrl.Result{RequeueAfter: pendingRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: refreshEvery}, nil
}

func ignoreRateLimit(err error) error {
	if _, ok := cloudflare.RetryAfter(err); ok {
		return nil
	}
	return err
}

// controllerAnnotations are removed when the controller lets go of a
// Gateway.
var controllerAnnotations = []string{
	config.AnnotationTunnel, config.AnnotationPreviousTunnel, config.AnnotationTokenSecret,
	config.AnnotationHostnames, config.AnnotationDNSHostnames, config.AnnotationGRPCNotified,
}

// routeSet holds every route in the cluster, by kind.
type routeSet struct {
	http []*gwv1.HTTPRoute
	grpc []*gwv1.GRPCRoute
	tcp  []*gwv1.TCPRoute
}

func (r *GatewayReconciler) listRoutes(ctx context.Context) (*routeSet, error) {
	rs := &routeSet{}
	var hl gwv1.HTTPRouteList
	if err := r.List(ctx, &hl); err != nil {
		return nil, err
	}
	for i := range hl.Items {
		rs.http = append(rs.http, &hl.Items[i])
	}
	if r.Features.GRPCRoute() {
		var l gwv1.GRPCRouteList
		if err := r.List(ctx, &l); err != nil {
			return nil, err
		}
		for i := range l.Items {
			rs.grpc = append(rs.grpc, &l.Items[i])
		}
	}
	if r.Features.TCPRoute() {
		var l gwv1.TCPRouteList
		if err := r.List(ctx, &l); err != nil {
			return nil, err
		}
		for i := range l.Items {
			rs.tcp = append(rs.tcp, &l.Items[i])
		}
	}
	return rs, nil
}

func (r *GatewayReconciler) translateInput(ctx context.Context, gw *gwv1.Gateway, rs *routeSet) (*translate.Input, error) {
	in := &translate.Input{
		Gateway:    gw,
		HTTPRoutes: rs.http, GRPCRoutes: rs.grpc, TCPRoutes: rs.tcp,
		ClusterDomain: r.Config.ClusterDomain,
		GRPCRoute:     r.Features.GRPCRoute(), TCPRoute: r.Features.TCPRoute(), TCPMode: r.Config.TCPMode,
		AllowedHostnames: allowedHostnamesFunc(ctx, r.Client, r.Config),
	}
	if r.Features.ReferenceGrant() {
		var l gwv1.ReferenceGrantList
		if err := r.List(ctx, &l); err != nil {
			return nil, err
		}
		for i := range l.Items {
			in.ReferenceGrants = append(in.ReferenceGrants, &l.Items[i])
		}
	}
	if needsNamespaceLabels(gw) {
		var nl corev1.NamespaceList
		if err := r.List(ctx, &nl); err != nil {
			return nil, err
		}
		in.NamespaceLabels = map[string]map[string]string{}
		for _, ns := range nl.Items {
			in.NamespaceLabels[ns.Name] = ns.Labels
		}
	}
	in.GetService = func(ns, name string) *corev1.Service {
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, svc); err != nil {
			return nil
		}
		return svc
	}
	claimed, err := r.claimedHostnames(ctx, gw)
	if err != nil {
		return nil, err
	}
	in.Claimed = claimed
	in.Generated = r.generatedRoute(ctx)
	return in, nil
}

// generatedRoute returns the translate.Input.Generated callback. Route
// authors can copy the labels, annotations and owner reference of a
// generated route, so a derived hostname is only trusted when it is the
// one the source's name and namespace derive, under the namespace's base
// domain; the DNS parent is further checked against the route's parents
// by DNS (dnsSyncer).
func (r *GatewayReconciler) generatedRoute(ctx context.Context) func(metav1.ObjectMeta) (map[string]bool, string) {
	bases := map[string]string{}
	base := func(ns string) string {
		b, ok := bases[ns]
		if !ok {
			b = r.Config.BaseDomain
			if pol, err := namespacePolicy(ctx, r.Client, r.Config, ns); err == nil && pol.BaseDomain != "" {
				b = pol.BaseDomain
			}
			bases[ns] = b
		}
		return b
	}
	return func(m metav1.ObjectMeta) (map[string]bool, string) {
		kind := m.Labels[config.LabelSourceKind]
		if m.Labels[connector.LabelManagedBy] != r.Config.Instance || (kind != config.SourceIngress && kind != config.SourceService) {
			return nil, ""
		}
		owner := metav1.GetControllerOfNoCopy(&m)
		if owner == nil || owner.Kind != map[string]string{config.SourceIngress: "Ingress", config.SourceService: "Service"}[kind] ||
			m.Labels[config.LabelSourceName] != LabelValue(owner.Name) {
			return nil, ""
		}
		var derived map[string]bool
		if b := base(m.Namespace); b != "" {
			allowed := map[string]bool{DefaultLabel(r.Config.HostnameTemplate, owner.Name, m.Namespace, "") + "." + b: true}
			if port, ok := strings.CutPrefix(m.Name, owner.Name+"-tcp-"); ok {
				allowed[DefaultLabel(r.Config.HostnameTemplate, owner.Name, m.Namespace, port)+"."+b] = true
			}
			for h := range translate.AnnotatedDerived(m) {
				if allowed[h] {
					if derived == nil {
						derived = map[string]bool{}
					}
					derived[h] = true
				}
			}
		}
		return derived, m.Annotations[config.AnnotationDNSParent]
	}
}

func needsNamespaceLabels(gw *gwv1.Gateway) bool {
	for _, l := range gw.Spec.Listeners {
		if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From != nil &&
			*l.AllowedRoutes.Namespaces.From == gwv1.NamespacesFromSelector {
			return true
		}
	}
	return false
}

// recordedDNSHostnames returns the hostnames whose DNS records gw may own:
// those in its record, plus any added to the annotation by hand (harmless,
// since records are only removed when their TXT record says they are
// gw's).
func recordedDNSHostnames(rec map[string]string, gw *gwv1.Gateway) []string {
	out := splitList(rec[config.AnnotationDNSHostnames])
	for _, h := range splitList(gw.Annotations[config.AnnotationDNSHostnames]) {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// release gives up the hostnames of a Gateway whose namespace no longer
// allows its class, which its author cannot undo: with the sync policy
// its DNS records are removed, and then the claim is dropped. Without
// usable credentials both are kept and a pendingError is returned. (A
// Gateway that is only not accepted keeps both.)
func (r *GatewayReconciler) release(ctx context.Context, gw *gwv1.Gateway) error {
	rec, err := r.recorded(ctx, gw)
	if err != nil {
		return err
	}
	ann := map[string]string{config.AnnotationHostnames: ""}
	if hosts := recordedDNSHostnames(rec, gw); len(hosts) > 0 && r.Config.DNSPolicy == config.DNSPolicySync {
		creds, err := r.gatewayCredentials(ctx, gw)
		var ip *invalidParamsError
		if err != nil && !asInvalid(err, &ip) {
			return err
		}
		if ip != nil {
			if creds = r.fallbackCredentials(); creds == nil {
				// Keep the claim until the records are gone, so that no
				// other Gateway runs into them.
				r.event(gw, corev1.EventTypeWarning, "CleanupBlocked",
					"cannot remove DNS records in Cloudflare (%s); fix the credentials", ip.msg)
				return &pendingError{ip.msg}
			}
		}
		s := &dnsSyncer{CF: r.CF, Config: r.Config, Token: creds.DNSToken, Gateway: gatewayKey(gw)}
		if err := s.RemoveAll(ctx, hosts); err != nil {
			return err
		}
		ann[config.AnnotationDNSHostnames] = ""
	}
	return r.setAnnotations(ctx, gw, ann)
}

// publishedElsewhere maps the other Gateways ("namespace/name") to the
// hostnames they publish and the routes that publish them, from their
// records.
func (r *GatewayReconciler) publishedElsewhere(ctx context.Context, gw *gwv1.Gateway) (map[string]map[string][]string, error) {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	recs, err := r.records(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]string{}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == gw.UID {
			continue
		}
		if hosts := parseHostnames(recs[other.UID][config.AnnotationHostnames]); len(hosts) > 0 {
			out[gatewayKey(other)] = hosts
		}
	}
	return out, nil
}

// claimedHostnames returns the hostnames other managed Gateways publish,
// from their records. The Gateway that publishes a hostname first keeps
// it: another Gateway's claim counts unless this Gateway publishes the
// hostname too and is the older of the two (which settles two Gateways
// that started publishing at once). A claim stands while its Gateway
// exists, even when the Gateway is not accepted for a while, so that a
// failed edit does not give its hostnames away.
func (r *GatewayReconciler) claimedHostnames(ctx context.Context, gw *gwv1.Gateway) (map[string]translate.Claim, error) {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	recs, err := r.records(ctx)
	if err != nil {
		return nil, err
	}
	// This Gateway's own record is read from the API server: the cache
	// may not have its latest write yet.
	mine, err := r.recorded(ctx, gw)
	if err != nil {
		return nil, err
	}
	own := parseHostnames(mine[config.AnnotationHostnames])
	claimed := map[string]translate.Claim{}
	classes := map[string]bool{}
	sort.Slice(list.Items, func(i, j int) bool { return olderGateway(&list.Items[i], &list.Items[j]) })
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == gw.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		hosts := parseHostnames(recs[other.UID][config.AnnotationHostnames])
		if len(hosts) == 0 {
			continue
		}
		cls := string(other.Spec.GatewayClassName)
		managed, seen := classes[cls]
		if !seen {
			gc, err := r.managedClass(ctx, cls)
			if err != nil {
				return nil, err
			}
			managed = gc != nil
			classes[cls] = managed
		}
		if !managed {
			continue
		}
		for host, routes := range hosts {
			if _, mine := own[host]; mine && olderGateway(gw, other) {
				continue
			}
			if _, taken := claimed[host]; taken {
				continue
			}
			c := translate.Claim{Gateway: other.Namespace + "/" + other.Name}
			for _, s := range routes {
				if ref, ok := translate.ParseRouteRef(s); ok {
					c.Routes = append(c.Routes, ref)
				}
			}
			claimed[host] = c
		}
	}
	return claimed, nil
}

func parseHostnames(raw string) map[string][]string {
	m := map[string][]string{}
	if raw == "" {
		return m
	}
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func formatHostnames(hosts []translate.Hostname) string {
	if len(hosts) == 0 {
		return ""
	}
	m := map[string][]string{}
	for _, h := range hosts {
		var routes []string
		for _, ref := range h.Routes {
			routes = append(routes, ref.String())
		}
		m[h.Name] = routes
	}
	b, _ := json.Marshal(m) // map keys are sorted
	return string(b)
}

// olderGateway orders Gateways by creation time, then by namespace/name.
func olderGateway(a, b *gwv1.Gateway) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}

// infraParams reads the Gateway's parameters ConfigMap.
func (r *GatewayReconciler) infraParams(ctx context.Context, gw *gwv1.Gateway) (connector.Params, error) {
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		return connector.DefaultParams(), nil
	}
	ref := gw.Spec.Infrastructure.ParametersRef
	if string(ref.Group) != "" || string(ref.Kind) != "ConfigMap" {
		return connector.DefaultParams(), nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: ref.Name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return connector.DefaultParams(), &invalidParamsError{fmt.Sprintf("infrastructure parameters ConfigMap %s/%s not found", gw.Namespace, ref.Name)}
		}
		return connector.DefaultParams(), err
	}
	p, unknown, err := connector.ParseParams(cm.Data)
	if err != nil {
		return connector.DefaultParams(), &invalidParamsError{fmt.Sprintf("infrastructure parameters ConfigMap %s/%s: %v", gw.Namespace, ref.Name, err)}
	}
	if len(unknown) > 0 {
		log.FromContext(ctx).Info("ignoring unknown infrastructure parameters", "configMap", ref.Name, "keys", unknown)
	}
	return p, nil
}

func ownerRef(gw *gwv1.Gateway) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         gwv1.GroupVersion.String(),
		Kind:               translate.KindGateway,
		Name:               gw.Name,
		UID:                gw.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// provision creates or updates the tunnel, its configuration, the
// connector and DNS.
func (r *GatewayReconciler) provision(ctx context.Context, gw *gwv1.Gateway, creds *Credentials, params connector.Params, res *translate.Result, out *outcome) error {
	tun, previous, err := r.ensureTunnel(ctx, gw, creds)
	if err != nil {
		return err
	}
	out.tunnel = tun
	if err := r.writeConfig(ctx, creds, tun.ID, res.Config); err != nil {
		return err
	}
	token, err := r.connectorToken(ctx, creds, tun.ID)
	if err != nil {
		return err
	}
	spec := &connector.Spec{
		Instance: r.Config.Instance, Namespace: gw.Namespace, Gateway: gw.Name,
		Image: r.Config.ConnectorImage, Params: params, Token: token,
		PodLabels: infrastructureLabels(gw), PodAnnotations: infrastructureAnnotations(gw), Owner: ownerRef(gw),
	}
	if err := r.applyOwned(ctx, gw, spec.Secret()); err != nil {
		return err
	}
	if params.Disabled {
		if err := r.withdraw(ctx, gw); err != nil {
			return err
		}
	} else {
		if spec.OmitReplicas, err = r.autoscaled(ctx, gw); err != nil {
			return err
		}
		if err := r.applyOwned(ctx, gw, spec.Deployment()); err != nil {
			return err
		}
		if pdb := spec.PodDisruptionBudget(); pdb != nil {
			if err := r.applyOwned(ctx, gw, pdb); err != nil {
				return err
			}
		} else if err := r.deleteOwned(ctx, gw, &policyv1.PodDisruptionBudget{}, connector.DeploymentName(gw.Name)); err != nil {
			return err
		}
		dep := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: connector.DeploymentName(gw.Name)}, dep); err == nil {
			out.ready = dep.Status.ReadyReplicas
			out.desired = ptr.Deref(dep.Spec.Replicas, params.Replicas)
		}
	}

	// DNS points at the new tunnel only once it has a connection; until
	// then a replaced tunnel keeps serving.
	target := tun.ID
	if previous != "" && tun.ActiveConnections() == 0 {
		target = previous
	}
	rec, err := r.recorded(ctx, gw)
	if err != nil {
		return err
	}
	recorded := recordedDNSHostnames(rec, gw)
	dnsHosts := recorded
	if out.dnsManaged {
		claims, err := r.publishedElsewhere(ctx, gw)
		if err != nil {
			return err
		}
		s := &dnsSyncer{CF: r.CF, Config: r.Config, Token: creds.DNSToken, Gateway: gatewayKey(gw), Target: target, Published: claims}
		out.dns, dnsHosts, out.dnsErr = s.Sync(ctx, res.Hostnames, recorded)
		if out.dns == nil {
			return out.dnsErr // rate limited
		}
		r.reportDNS(gw, res.Hostnames, out.dns)
	} else if len(recorded) > 0 && r.Config.DNSPolicy == config.DNSPolicySync {
		s := &dnsSyncer{CF: r.CF, Config: r.Config, Token: creds.DNSToken, Gateway: gatewayKey(gw)}
		if err := s.RemoveAll(ctx, recorded); err != nil {
			return err
		}
		dnsHosts = nil
	}

	if err := r.syncAccess(ctx, gw, creds, res.Hostnames); err != nil {
		return err
	}

	ann := map[string]string{
		config.AnnotationTunnel:       tun.ID,
		config.AnnotationTokenSecret:  connector.SecretName(gw.Name),
		config.AnnotationHostnames:    formatHostnames(res.Hostnames),
		config.AnnotationDNSHostnames: strings.Join(dnsHosts, ","),
	}
	if res.GRPC && gw.Annotations[config.AnnotationGRPCNotified] == "" {
		r.event(gw, corev1.EventTypeNormal, "GRPCRouteAttached",
			"a GRPCRoute is attached: enable gRPC in the zone's Network settings in Cloudflare, and serve TLS from the backend (appProtocol: https)")
		ann[config.AnnotationGRPCNotified] = "true"
	}
	if !r.Config.AccessApps {
		old := parseHostnames(gw.Annotations[config.AnnotationHostnames])
		for _, h := range res.Hostnames {
			if _, seen := old[h.Name]; h.TCP && !seen {
				r.event(gw, corev1.EventTypeNormal, "TCPHostnamePublished",
					"%s is published for TCP: clients connect with `cloudflared access tcp`; protect it with a Cloudflare Access application", h.Name)
			}
		}
	}

	// The replaced tunnel goes once the new one serves and DNS follows.
	if previous != "" {
		ann[config.AnnotationPreviousTunnel] = previous
		dnsDone := !out.dnsManaged || out.dnsErr == nil
		if r.programmed(out) && dnsDone {
			if gw.Annotations[config.AnnotationKeepTunnel] != "true" {
				if err := r.deleteTunnel(ctx, gw, creds, previous); err != nil {
					return err
				}
			}
			r.event(gw, corev1.EventTypeNormal, "TunnelReplaced", "now served by tunnel %s; tunnel %s is released", tun.ID, previous)
			ann[config.AnnotationPreviousTunnel] = ""
		}
	}
	return r.setAnnotations(ctx, gw, ann)
}

// reportDNS emits events for hostnames whose records conflict.
func (r *GatewayReconciler) reportDNS(gw *gwv1.Gateway, hosts []translate.Hostname, states map[string]dnsState) {
	var conflicts []string
	for _, h := range hosts {
		if st, ok := states[h.Name]; ok && !st.Ready && strings.Contains(st.Message, "does not own") {
			conflicts = append(conflicts, st.Message)
		}
	}
	if len(conflicts) > 0 {
		r.event(gw, corev1.EventTypeWarning, "DNSConflict", "%s", strings.Join(conflicts, "; "))
	}
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// setAnnotations patches controller annotations; "" removes one. The
// recorded annotations are written to the Gateway's record first, and the
// Gateway's annotations then mirror the record, replacing any value
// written by hand.
func (r *GatewayReconciler) setAnnotations(ctx context.Context, gw *gwv1.Gateway, ann map[string]string) error {
	rec, err := r.recorded(ctx, gw)
	if err != nil {
		return err
	}
	changed := false
	for _, k := range recordedAnnotations {
		if v, set := ann[k]; set && rec[k] != v {
			if v == "" {
				delete(rec, k)
			} else {
				rec[k] = v
			}
			changed = true
		}
	}
	if changed {
		if err := r.writeRecord(ctx, gw, rec); err != nil {
			return err
		}
	}
	merged := map[string]string{}
	for k, v := range gw.Annotations {
		merged[k] = v
	}
	for k, v := range ann {
		if v == "" {
			delete(merged, k)
		} else {
			merged[k] = v
		}
	}
	for _, k := range recordedAnnotations {
		if v := rec[k]; v != "" {
			merged[k] = v
		} else {
			delete(merged, k)
		}
	}
	if maps.Equal(merged, gw.Annotations) || len(merged) == 0 && len(gw.Annotations) == 0 {
		return nil
	}
	orig := gw.DeepCopy()
	gw.Annotations = merged
	return r.Patch(ctx, gw, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// autoscaled reports whether a HorizontalPodAutoscaler targets the
// connector Deployment.
func (r *GatewayReconciler) autoscaled(ctx context.Context, gw *gwv1.Gateway) (bool, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := r.reader().List(ctx, &list, client.InNamespace(gw.Namespace)); err != nil {
		if apierrors.IsForbidden(err) || isNoKindMatch(err) {
			return false, nil
		}
		return false, err
	}
	for _, h := range list.Items {
		t := h.Spec.ScaleTargetRef
		if t.Kind == "Deployment" && t.Name == connector.DeploymentName(gw.Name) {
			return true, nil
		}
	}
	return false, nil
}

// applyOwned applies an object of the Gateway's connector. An object of
// the same name that the Gateway does not control is left alone.
func (r *GatewayReconciler) applyOwned(ctx context.Context, gw *gwv1.Gateway, obj client.Object) error {
	err := applyControlled(ctx, r.Client, r.reader(), r.Scheme, obj, gw)
	var ce *conflictError
	if errors.As(err, &ce) {
		return &pendingError{strings.Replace(ce.msg, "this object", "this Gateway", 1)}
	}
	return err
}

// infrastructureLabels are the labels the Gateway asks for on its
// generated resources (spec.infrastructure.labels).
func infrastructureLabels(gw *gwv1.Gateway) map[string]string {
	if gw.Spec.Infrastructure == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range gw.Spec.Infrastructure.Labels {
		out[string(k)] = string(v)
	}
	return out
}

func infrastructureAnnotations(gw *gwv1.Gateway) map[string]string {
	if gw.Spec.Infrastructure == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range gw.Spec.Infrastructure.Annotations {
		out[string(k)] = string(v)
	}
	return out
}

// withdraw stops the connector: its Deployment and PodDisruptionBudget are
// deleted. The tunnel and token Secret are kept.
func (r *GatewayReconciler) withdraw(ctx context.Context, gw *gwv1.Gateway) error {
	return errors.Join(
		r.deleteOwned(ctx, gw, &appsv1.Deployment{}, connector.DeploymentName(gw.Name)),
		r.deleteOwned(ctx, gw, &policyv1.PodDisruptionBudget{}, connector.DeploymentName(gw.Name)),
	)
}

func (r *GatewayReconciler) deleteOwned(ctx context.Context, gw *gwv1.Gateway, obj client.Object, name string) error {
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: name}, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, gw) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

// programmed reports whether the Gateway serves traffic.
func (r *GatewayReconciler) programmed(out *outcome) bool {
	if out.tunnel == nil {
		return false
	}
	if out.disabled {
		return true
	}
	return out.ready > 0 && out.tunnel.ActiveConnections() > 0
}

// ---- finalization ----

// finalize removes what the Gateway owns in Cloudflare. unmanage is set
// when the Gateway moved to another controller's class: its Kubernetes
// objects are deleted too, since owner references keep them otherwise.
func (r *GatewayReconciler) finalize(ctx context.Context, gw *gwv1.Gateway, unmanage bool) (bool, error) {
	if err := r.writeFinalizingStatus(ctx, gw); err != nil {
		return false, err
	}
	if gw.Annotations[config.AnnotationSkipCleanup] != "true" {
		if err := r.cleanupCloudflare(ctx, gw); err != nil {
			var pe *pendingError
			if errors.As(err, &pe) {
				return false, nil
			}
			return false, err
		}
	}
	rs, err := r.listRoutes(ctx)
	if err != nil {
		return false, err
	}
	if err := r.writeRouteStatus(ctx, gw, rs, &translate.Result{}, nil); err != nil {
		return false, err
	}
	if unmanage {
		if err := errors.Join(
			r.withdraw(ctx, gw),
			r.deleteOwned(ctx, gw, &corev1.Secret{}, connector.SecretName(gw.Name)),
		); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *GatewayReconciler) cleanupCloudflare(ctx context.Context, gw *gwv1.Gateway) error {
	// Only tunnel IDs the controller recorded are candidates; the
	// Gateway's own tunnel is also found by name below. DNS records are
	// checked against their ownership TXT record, so the list of names to
	// look at may also come from the annotation.
	rec, err := r.recorded(ctx, gw)
	if err != nil {
		return err
	}
	tunnels := splitList(rec[config.AnnotationTunnel] + "," + rec[config.AnnotationPreviousTunnel])
	dnsHosts := recordedDNSHostnames(rec, gw)
	creds, err := r.gatewayCredentials(ctx, gw)
	var ip *invalidParamsError
	if err != nil && !asInvalid(err, &ip) {
		return err
	}
	if ip != nil && len(tunnels) == 0 && len(dnsHosts) == 0 {
		return nil // nothing recorded, and no credentials to look for more
	}
	if ip != nil {
		if creds = r.fallbackCredentials(); creds == nil {
			r.event(gw, corev1.EventTypeWarning, "CleanupBlocked",
				"cannot clean up in Cloudflare (%s); fix the credentials, or set %s: \"true\" to skip cleanup", ip.msg, config.AnnotationSkipCleanup)
			return &pendingError{ip.msg}
		}
	}
	if r.Config.DNSPolicy == config.DNSPolicySync && len(dnsHosts) > 0 {
		s := &dnsSyncer{CF: r.CF, Config: r.Config, Token: creds.DNSToken, Gateway: gatewayKey(gw)}
		if err := s.RemoveAll(ctx, dnsHosts); err != nil {
			return err
		}
	}
	if err := r.deleteAccessApps(ctx, gw, creds, nil); err != nil {
		return err
	}
	if gw.Annotations[config.AnnotationKeepTunnel] == "true" {
		return nil
	}
	// A tunnel created just before its ID could be recorded is found by
	// its name.
	if named, err := r.CF.ListTunnels(ctx, creds.AccountID, creds.AccountToken, r.tunnelName(gw)); err != nil {
		return err
	} else {
		for _, t := range named {
			if t.Name == r.tunnelName(gw) && !t.Deleted() && !slices.Contains(tunnels, t.ID) {
				tunnels = append(tunnels, t.ID)
			}
		}
	}
	for _, id := range tunnels {
		if err := r.deleteTunnel(ctx, gw, creds, id); err != nil {
			return err
		}
	}
	return nil
}

// writeClassMissingStatus marks a Gateway whose GatewayClass no longer
// exists.
func (r *GatewayReconciler) writeClassMissingStatus(ctx context.Context, gw *gwv1.Gateway) error {
	orig := gw.DeepCopy()
	msg := fmt.Sprintf("GatewayClass %q not found; the tunnel and connector are kept until the class is back", gw.Spec.GatewayClassName)
	for _, t := range []gwv1.GatewayConditionType{gwv1.GatewayConditionAccepted, gwv1.GatewayConditionProgrammed} {
		meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{Type: string(t), Status: metav1.ConditionFalse,
			Reason: string(gwv1.GatewayReasonPending), Message: msg, ObservedGeneration: gw.Generation})
	}
	if gatewayStatusEqual(&orig.Status, &gw.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, gw, client.MergeFrom(orig)))
}

func (r *GatewayReconciler) writeFinalizingStatus(ctx context.Context, gw *gwv1.Gateway) error {
	orig := gw.DeepCopy()
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{Type: string(gwv1.GatewayConditionAccepted), Status: metav1.ConditionUnknown,
		Reason: string(gwv1.GatewayReasonPending), Message: "cleaning up the tunnel and DNS records", ObservedGeneration: gw.Generation})
	if gatewayStatusEqual(&orig.Status, &gw.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, gw, client.MergeFrom(orig)))
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
