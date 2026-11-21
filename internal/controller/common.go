// Package controller holds the reconcilers.
package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// FieldOwner is the server-side apply field manager.
const FieldOwner = "cf-tunnel-gateway"

// CacheOptions limits what the manager's informers hold. Secrets are never
// cached in full: the controller watches Secret metadata only (to notice
// credential changes) and reads Secrets from the API server.
func CacheOptions(cfg *config.Config) cache.Options {
	o := cache.Options{}
	if len(cfg.WatchNamespaces) > 0 {
		o.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range cfg.WatchNamespaces {
			o.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	return o
}

// ClientOptions makes the manager's client read rarely needed objects
// directly instead of caching them.
func ClientOptions() client.Options {
	return client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{
		&corev1.Secret{}, &policyv1.PodDisruptionBudget{}, &autoscalingv2.HorizontalPodAutoscaler{},
	}}}
}

// Features records which optional Gateway API kinds the cluster serves.
// Kinds can appear while the controller runs (see FeatureWatcher).
type Features struct {
	grpc, tcp, grant atomic.Bool
}

// GRPCRoute reports whether GRPCRoute is served.
func (f *Features) GRPCRoute() bool { return f.grpc.Load() }

// TCPRoute reports whether TCPRoute is served.
func (f *Features) TCPRoute() bool { return f.tcp.Load() }

// ReferenceGrant reports whether ReferenceGrant is served.
func (f *Features) ReferenceGrant() bool { return f.grant.Load() }

// Set records the served kinds.
func (f *Features) Set(grpc, tcp, grant bool) {
	f.grpc.Store(grpc)
	f.tcp.Store(tcp)
	f.grant.Store(grant)
}

// AllFeatures enables every optional kind (tests).
func AllFeatures() *Features {
	f := &Features{}
	f.Set(true, true, true)
	return f
}

// ErrMissingCoreCRDs is returned by DetectFeatures when GatewayClass,
// Gateway or HTTPRoute is not served at v1.
type ErrMissingCoreCRDs struct{ Missing []string }

func (e *ErrMissingCoreCRDs) Error() string {
	return fmt.Sprintf("Gateway API v1.6.0 or newer (Standard channel) is required; %s not served at %s/v1", e.Missing, gwv1.GroupName)
}

// DetectFeatures checks which Gateway API kinds are served at v1.
func DetectFeatures(dc discovery.DiscoveryInterface) (grpc, tcp, grant bool, err error) {
	kinds := map[string]bool{}
	list, err := dc.ServerResourcesForGroupVersion(gwv1.GroupVersion.String())
	if err != nil && !apierrors.IsNotFound(err) {
		return false, false, false, err
	}
	if list != nil {
		for _, r := range list.APIResources {
			kinds[r.Kind] = true
		}
	}
	var missing []string
	for _, k := range []string{"GatewayClass", "Gateway", "HTTPRoute"} {
		if !kinds[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return false, false, false, &ErrMissingCoreCRDs{Missing: missing}
	}
	return kinds["GRPCRoute"], kinds["TCPRoute"], kinds["ReferenceGrant"], nil
}

// FeatureWatcher rechecks the served kinds every minute and calls the
// registered functions when an optional kind appears, so that installing
// it later enables the feature without a restart. Kinds that disappear
// stay enabled until the next restart (their informers would fail anyway).
type FeatureWatcher struct {
	Discovery discovery.DiscoveryInterface
	Features  *Features
	Interval  time.Duration

	mu      sync.Mutex
	onGRPC  []func() error
	onTCP   []func() error
	onGrant []func() error
}

// OnGRPCRoute registers fn to run when GRPCRoute becomes available.
func (w *FeatureWatcher) OnGRPCRoute(fn func() error) {
	w.mu.Lock()
	w.onGRPC = append(w.onGRPC, fn)
	w.mu.Unlock()
}

// OnTCPRoute registers fn to run when TCPRoute becomes available.
func (w *FeatureWatcher) OnTCPRoute(fn func() error) {
	w.mu.Lock()
	w.onTCP = append(w.onTCP, fn)
	w.mu.Unlock()
}

// OnReferenceGrant registers fn to run when ReferenceGrant becomes available.
func (w *FeatureWatcher) OnReferenceGrant(fn func() error) {
	w.mu.Lock()
	w.onGrant = append(w.onGrant, fn)
	w.mu.Unlock()
}

// Start implements manager.Runnable.
func (w *FeatureWatcher) Start(ctx context.Context) error {
	interval := w.Interval
	if interval == 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.check(ctx)
		}
	}
}

func (w *FeatureWatcher) check(ctx context.Context) {
	grpc, tcp, grant, err := DetectFeatures(w.Discovery)
	if err != nil {
		log.FromContext(ctx).Error(err, "rechecking Gateway API kinds")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	run := func(kind string, fns []func() error) {
		log.FromContext(ctx).Info("optional Gateway API kind is now served; enabling it", "kind", kind)
		for _, fn := range fns {
			if err := fn(); err != nil {
				log.FromContext(ctx).Error(err, "enabling", "kind", kind)
			}
		}
	}
	if grpc && !w.Features.GRPCRoute() {
		w.Features.grpc.Store(true)
		run("GRPCRoute", w.onGRPC)
	}
	if tcp && !w.Features.TCPRoute() {
		w.Features.tcp.Store(true)
		run("TCPRoute", w.onTCP)
	}
	if grant && !w.Features.ReferenceGrant() {
		w.Features.grant.Store(true)
		run("ReferenceGrant", w.onGrant)
	}
}

// ---- applying objects ----

// conflictError is an object name already used by something else.
type conflictError struct{ msg string }

func (e *conflictError) Error() string { return e.msg }

// applyControlled server-side applies obj only if no object of that name
// exists, or the existing one is controlled by owner; otherwise it returns
// a *conflictError and leaves the object alone (taking it over would make
// garbage collection delete it with owner). The existing object's UID is
// sent as a precondition, so an object replaced in between is not taken
// over either.
func applyControlled(ctx context.Context, c client.Client, rd client.Reader, scheme *runtime.Scheme, obj client.Object, owner metav1.Object) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return err
	}
	cur := obj.DeepCopyObject().(client.Object)
	var uid types.UID
	switch err := rd.Get(ctx, client.ObjectKeyFromObject(obj), cur); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case !metav1.IsControlledBy(cur, owner):
		return &conflictError{fmt.Sprintf("%s %s/%s exists and is not owned by this object", gvk.Kind, obj.GetNamespace(), obj.GetName())}
	default:
		uid = cur.GetUID()
	}
	if err := applyWithGVK(ctx, c, obj, gvk, uid); err != nil {
		return fmt.Errorf("applying %s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

// apply server-side applies obj, a fully specified typed object.
func apply(ctx context.Context, c client.Client, scheme *runtime.Scheme, obj client.Object) error {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return err
	}
	return applyWithGVK(ctx, c, obj, gvk, "")
}

func applyWithGVK(ctx context.Context, c client.Client, obj client.Object, gvk schema.GroupVersionKind, uid types.UID) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(gvk)
	delete(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")
	if uid != "" {
		u.SetUID(uid)
	}
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// ---- small helpers ----

func gatewayKey(gw *gwv1.Gateway) string { return gw.Namespace + "/" + gw.Name }

// gatewayParentRef is the parentRef form of a Gateway.
func gatewayParentRef(ns, name string) gwv1.ParentReference {
	g := gwv1.Group(gwv1.GroupName)
	k := gwv1.Kind(translate.KindGateway)
	n := gwv1.Namespace(ns)
	return gwv1.ParentReference{Group: &g, Kind: &k, Namespace: &n, Name: gwv1.ObjectName(name)}
}

func equalConditions(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message || x.ObservedGeneration != y.ObservedGeneration {
			return false
		}
	}
	return true
}

func isNoKindMatch(err error) bool { return meta.IsNoMatchError(err) }

// managedByLabels mark objects of this installation.
func managedByLabels(cfg *config.Config) map[string]string {
	return map[string]string{connector.LabelManagedBy: cfg.Instance}
}

func labelSelector(m map[string]string) labels.Selector { return labels.SelectorFromSet(m) }

// conditionTrue reports whether a condition is True for the given
// generation.
func conditionTrue(conds []metav1.Condition, typ string, gen int64) bool {
	c := meta.FindStatusCondition(conds, typ)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == gen
}
