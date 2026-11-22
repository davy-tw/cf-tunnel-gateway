package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// revalidateEvery is how often a class's tokens are verified again, so
// that revoked or expired tokens surface.
const revalidateEvery = time.Hour

// retryFailedEvery is how long a failed verification is remembered, so
// that a transient Cloudflare error does not reject a class for long.
const retryFailedEvery = time.Minute

// GatewayClassReconciler validates managed GatewayClasses.
type GatewayClassReconciler struct {
	classResolver

	mu       sync.Mutex
	verified map[types.UID]verification
}

type verification struct {
	key string
	at  time.Time
	err error
}

// NewGatewayClassReconciler returns a reconciler.
func NewGatewayClassReconciler(c client.Client, cfg *config.Config, cf *cloudflare.Client) *GatewayClassReconciler {
	return &GatewayClassReconciler{classResolver: classResolver{Client: c, Config: cfg, CF: cf}, verified: map[types.UID]verification{}}
}

// Reconcile implements reconcile.Reconciler.
func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gc, err := r.managedClass(ctx, req.Name)
	if err != nil || gc == nil {
		return ctrl.Result{}, err
	}
	if err := r.syncFinalizer(ctx, gc); err != nil {
		return ctrl.Result{}, err
	}
	if !gc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	accepted := metav1.Condition{
		Type: string(gwv1.GatewayClassConditionStatusAccepted), Status: metav1.ConditionTrue,
		Reason: string(gwv1.GatewayClassReasonAccepted), Message: "GatewayClass accepted: Cloudflare credentials verified", ObservedGeneration: gc.Generation,
	}
	requeue := revalidateEvery
	creds, cerr := r.credentials(ctx, gc)
	var ip *invalidParamsError
	switch {
	case cerr == nil:
		if verr := r.verify(ctx, gc, creds); verr != nil {
			if d, ok := cloudflare.RetryAfter(verr); ok {
				return ctrl.Result{RequeueAfter: d}, nil
			}
			accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionFalse, string(gwv1.GatewayClassReasonInvalidParameters), verr.Error()
			requeue = time.Minute
		}
	case asInvalid(cerr, &ip):
		accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionFalse, string(gwv1.GatewayClassReasonInvalidParameters), ip.msg
	default:
		return ctrl.Result{}, cerr
	}
	orig := gc.DeepCopy()
	meta.SetStatusCondition(&gc.Status.Conditions, accepted)
	meta.SetStatusCondition(&gc.Status.Conditions, r.supportedVersion(ctx, gc))
	if equalConditions(orig.Status.Conditions, gc.Status.Conditions) {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if err := r.Status().Patch(ctx, gc, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{RequeueAfter: time.Minute}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// verify checks the class's tokens, remembering the result for an hour
// per Secret version and class generation.
func (r *GatewayClassReconciler) verify(ctx context.Context, gc *gwv1.GatewayClass, c *Credentials) error {
	key := fmt.Sprintf("%d|%s|%s", gc.Generation, c.SecretVersion, tokenKey(c.AccountToken+"|"+r.Config.APIToken))
	r.mu.Lock()
	v, ok := r.verified[gc.UID]
	r.mu.Unlock()
	ttl := revalidateEvery
	if v.err != nil {
		ttl = retryFailedEvery
	}
	if ok && v.key == key && time.Since(v.at) < ttl {
		return v.err
	}
	err := r.check(ctx, c)
	if _, limited := cloudflare.RetryAfter(err); !limited {
		r.mu.Lock()
		r.verified[gc.UID] = verification{key: key, at: time.Now(), err: err}
		r.mu.Unlock()
	}
	return err
}

func (r *GatewayClassReconciler) check(ctx context.Context, c *Credentials) error {
	if err := r.CF.VerifyToken(ctx, c.AccountToken, c.AccountID); err != nil {
		return tokenProblem("account API token", err)
	}
	if err := r.CF.CanListTunnels(ctx, c.AccountID, c.AccountToken); err != nil {
		return tokenProblem("account API token cannot list the account's tunnels (it needs \"Cloudflare Tunnel: Edit\")", err)
	}
	if c.DNSToken != c.AccountToken {
		if err := r.CF.VerifyToken(ctx, c.DNSToken, c.AccountID); err != nil {
			return tokenProblem("DNS API token", err)
		}
	}
	n, err := r.CF.CountZones(ctx, c.DNSToken, "")
	if err != nil {
		return tokenProblem("DNS API token cannot list zones", err)
	}
	if n == 0 {
		return errors.New("the DNS API token cannot see any zone (it needs \"Zone: DNS: Edit\" and \"Zone: Zone: Read\" on the zones to publish in)")
	}
	return nil
}

func tokenProblem(what string, err error) error {
	if _, limited := cloudflare.RetryAfter(err); limited {
		return err
	}
	return fmt.Errorf("%s: %v", what, err)
}

var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// supportedVersion reports whether the installed Gateway API bundle is one
// this controller supports (v1.6 or newer v1 releases).
func (r *GatewayClassReconciler) supportedVersion(ctx context.Context, gc *gwv1.GatewayClass) metav1.Condition {
	c := metav1.Condition{Type: string(gwv1.GatewayClassConditionStatusSupportedVersion), ObservedGeneration: gc.Generation}
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	version := ""
	if err := r.reader().Get(ctx, types.NamespacedName{Name: "gatewayclasses.gateway.networking.k8s.io"}, crd); err == nil {
		version = crd.GetAnnotations()["gateway.networking.k8s.io/bundle-version"]
	}
	if SupportedBundleVersion(version) {
		c.Status, c.Reason, c.Message = metav1.ConditionTrue, string(gwv1.GatewayClassReasonSupportedVersion), "Gateway API "+version+" is supported"
	} else {
		if version == "" {
			version = "unknown"
		}
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, string(gwv1.GatewayClassReasonUnsupportedVersion),
			fmt.Sprintf("Gateway API bundle version %s is not supported; v1.6.0 or newer v1 releases are", version)
	}
	return c
}

// SupportedBundleVersion reports whether a bundle version ("v1.6.3") is
// v1.6 or a newer v1 release.
func SupportedBundleVersion(v string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(v, "v"), "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major == 1 && minor >= 6
}

// syncFinalizer keeps the standard gateway-exists finalizer on a class
// while Gateways use it.
func (r *GatewayClassReconciler) syncFinalizer(ctx context.Context, gc *gwv1.GatewayClass) error {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	inUse := false
	for _, gw := range list.Items {
		if string(gw.Spec.GatewayClassName) == gc.Name {
			inUse = true
			break
		}
	}
	has := controllerutil.ContainsFinalizer(gc, gwv1.GatewayClassFinalizerGatewaysExist)
	if inUse == has || inUse && !gc.DeletionTimestamp.IsZero() {
		return nil
	}
	orig := gc.DeepCopy()
	if inUse {
		controllerutil.AddFinalizer(gc, gwv1.GatewayClassFinalizerGatewaysExist)
	} else {
		controllerutil.RemoveFinalizer(gc, gwv1.GatewayClassFinalizerGatewaysExist)
	}
	return client.IgnoreNotFound(r.Patch(ctx, gc, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
}

// gatewayClassUsers enqueues the class of a Gateway that is created or
// deleted, and both classes when a Gateway changes class.
func gatewayClassUsers() handler.EventHandler {
	add := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], obj client.Object) {
		if gw, ok := obj.(*gwv1.Gateway); ok {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}})
		}
	}
	return handler.Funcs{
		CreateFunc: func(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(q, e.Object)
		},
		UpdateFunc: func(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			o, n := e.ObjectOld.(*gwv1.Gateway), e.ObjectNew.(*gwv1.Gateway)
			if o.Spec.GatewayClassName != n.Spec.GatewayClassName {
				add(q, o)
				add(q, n)
			}
		},
		DeleteFunc: func(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			add(q, e.Object)
		},
	}
}

// classesForSecret maps a credentials Secret to the classes using it.
func classesForSecret(c client.Client, controllerName string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list gwv1.GatewayClassList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, gc := range list.Items {
			if string(gc.Spec.ControllerName) != controllerName {
				continue
			}
			if ref := gc.Spec.ParametersRef; ref != nil && string(ref.Kind) == "Secret" && ref.Name == obj.GetName() &&
				ref.Namespace != nil && string(*ref.Namespace) == obj.GetNamespace() {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: gc.Name}})
			}
		}
		return out
	}
}

// secretMetadata is the metadata-only form of Secrets the controller
// watches: their contents never enter the cache.
func secretMetadata() *metav1.PartialObjectMetadata {
	m := &metav1.PartialObjectMetadata{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	return m
}

// SetupWithManager registers the reconciler.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
	return ctrl.NewControllerManagedBy(mgr).
		Named("gatewayclass").
		For(&gwv1.GatewayClass{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			gc, ok := o.(*gwv1.GatewayClass)
			return ok && string(gc.Spec.ControllerName) == r.Config.ControllerName
		}))).
		WatchesMetadata(secretMetadata(), handler.EnqueueRequestsFromMapFunc(classesForSecret(r.Client, r.Config.ControllerName)),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{})).
		Watches(&gwv1.Gateway{}, gatewayClassUsers()).
		Complete(r)
}
