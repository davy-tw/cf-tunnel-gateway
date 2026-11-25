package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// ---- Gateway status ----

func (r *GatewayReconciler) writeGatewayStatus(ctx context.Context, gw *gwv1.Gateway, res *translate.Result, ip *invalidParamsError, progErr error, out *outcome) error {
	orig := gw.DeepCopy()
	st := &gw.Status
	gen := gw.Generation

	accepted := metav1.Condition{Type: string(gwv1.GatewayConditionAccepted), ObservedGeneration: gen}
	switch {
	case ip != nil:
		accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionFalse, string(gwv1.GatewayReasonInvalidParameters), ip.msg
	case !res.Accepted:
		accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionFalse, res.AcceptedReason, res.AcceptedMessage
	default:
		accepted.Status, accepted.Reason, accepted.Message = metav1.ConditionTrue, res.AcceptedReason, res.AcceptedMessage
	}
	if accepted.Reason == "" {
		accepted.Reason = string(gwv1.GatewayReasonAccepted)
	}
	if accepted.Message == "" {
		accepted.Message = "Gateway accepted"
	}
	meta.SetStatusCondition(&st.Conditions, accepted)

	programmed := metav1.Condition{Type: string(gwv1.GatewayConditionProgrammed), ObservedGeneration: gen,
		Status: metav1.ConditionFalse, Reason: string(gwv1.GatewayReasonPending)}
	counts := fmt.Sprintf("%d/%d connector replicas ready", out.ready, out.desired)
	switch {
	case ip != nil || !res.Accepted:
		programmed.Reason = string(gwv1.GatewayReasonInvalid)
		programmed.Message = "Gateway is not accepted"
		if progErr != nil {
			programmed.Message = progErr.Error()
		}
	case out.tunnel == nil:
		programmed.Message = "setting up the tunnel"
		if progErr != nil {
			programmed.Message = progErr.Error()
		}
	case progErr != nil:
		programmed.Message = progErr.Error()
	case out.disabled:
		programmed.Status, programmed.Reason = metav1.ConditionTrue, string(gwv1.GatewayReasonProgrammed)
		programmed.Message = fmt.Sprintf("tunnel %s is configured; the connector is disabled, run it with the token in Secret %s (%d active connections)",
			out.tunnel.ID, connector.SecretName(gw.Name), out.tunnel.ActiveConnections())
	case out.ready == 0:
		programmed.Message = "waiting for a connector replica to become ready (" + counts + ")"
	case out.tunnel.ActiveConnections() == 0:
		programmed.Message = "waiting for tunnel " + out.tunnel.ID + " to report a connection (" + counts + ")"
	default:
		programmed.Status, programmed.Reason = metav1.ConditionTrue, string(gwv1.GatewayReasonProgrammed)
		programmed.Message = fmt.Sprintf("%s; tunnel %s has %d active connections", counts, out.tunnel.ID, out.tunnel.ActiveConnections())
		if out.dnsErr != nil {
			programmed.Message += fmt.Sprintf("; updating DNS failed: %v", out.dnsErr)
		}
	}
	meta.SetStatusCondition(&st.Conditions, programmed)

	var listeners []gwv1.ListenerStatus
	for _, lr := range res.Listeners {
		var prev []metav1.Condition
		for _, old := range orig.Status.Listeners {
			if old.Name == lr.Name {
				prev = old.Conditions
			}
		}
		conds, kinds := listenerConditions(prev, lr, programmed, gen)
		listeners = append(listeners, gwv1.ListenerStatus{Name: lr.Name, SupportedKinds: kinds, AttachedRoutes: lr.AttachedRoutes, Conditions: conds})
	}
	st.Listeners = listeners
	if r.Features.ListenerSet() {
		n := res.AttachedListenerSets
		if ip != nil {
			n = 0 // see writeListenerSetStatus
		}
		st.AttachedListenerSets = ptr.To(n)
	}

	st.Addresses = nil
	if out.tunnel != nil {
		st.Addresses = []gwv1.GatewayStatusAddress{{Type: ptr.To(gwv1.HostnameAddressType), Value: cloudflare.CNAMETarget(out.tunnel.ID)}}
	}
	if gatewayStatusEqual(&orig.Status, st) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, gw, client.MergeFrom(orig)))
}

func gatewayStatusEqual(a, b *gwv1.GatewayStatus) bool {
	if !equalConditions(a.Conditions, b.Conditions) || len(a.Listeners) != len(b.Listeners) || len(a.Addresses) != len(b.Addresses) ||
		ptr.Deref(a.AttachedListenerSets, -1) != ptr.Deref(b.AttachedListenerSets, -1) {
		return false
	}
	for i := range a.Addresses {
		if a.Addresses[i].Value != b.Addresses[i].Value {
			return false
		}
	}
	for i := range a.Listeners {
		x, y := a.Listeners[i], b.Listeners[i]
		if x.Name != y.Name || x.AttachedRoutes != y.AttachedRoutes || len(x.SupportedKinds) != len(y.SupportedKinds) || !equalConditions(x.Conditions, y.Conditions) {
			return false
		}
		for j := range x.SupportedKinds {
			if x.SupportedKinds[j].Kind != y.SupportedKinds[j].Kind {
				return false
			}
		}
	}
	return true
}

// ---- route status ----

type routeStatusTarget struct {
	obj    client.Object
	kind   string
	status *gwv1.RouteStatus
}

func (rs *routeSet) targets() []routeStatusTarget {
	var out []routeStatusTarget
	for _, r := range rs.http {
		out = append(out, routeStatusTarget{r, translate.KindHTTPRoute, &r.Status.RouteStatus})
	}
	for _, r := range rs.grpc {
		out = append(out, routeStatusTarget{r, translate.KindGRPCRoute, &r.Status.RouteStatus})
	}
	for _, r := range rs.tcp {
		out = append(out, routeStatusTarget{r, translate.KindTCPRoute, &r.Status.RouteStatus})
	}
	return out
}

// dnsCondition is the DNSRecordsReady condition of a route parent, or nil
// when DNS is not managed.
func dnsCondition(pr translate.ParentResult, out *outcome, gen int64) *metav1.Condition {
	if out == nil || !out.dnsManaged || len(pr.Hostnames) == 0 {
		return nil
	}
	if c := meta.FindStatusCondition(pr.Conditions, string(gwv1.RouteConditionAccepted)); c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	if out.dns == nil {
		c := translate.Cond(translate.ConditionDNSRecordsReady, false, "Pending", "waiting for the tunnel", gen)
		return &c
	}
	var problems []string
	for _, h := range pr.Hostnames {
		st, ok := out.dns[h]
		switch {
		case !ok:
			problems = append(problems, h+" is not published")
		case !st.Ready:
			problems = append(problems, st.Message)
		}
	}
	if len(problems) > 0 {
		reason := "Pending"
		for _, p := range problems {
			if strings.Contains(p, "does not own") || strings.Contains(p, "are owned by") {
				reason = translate.ReasonConflicted
			} else if strings.Contains(p, "no active Cloudflare zone") {
				reason = "ZoneNotFound"
			}
		}
		c := translate.Cond(translate.ConditionDNSRecordsReady, false, reason, strings.Join(problems, "; "), gen)
		return &c
	}
	c := translate.Cond(translate.ConditionDNSRecordsReady, true, "Ready",
		fmt.Sprintf("DNS records of %s point at the tunnel", strings.Join(pr.Hostnames, ", ")), gen)
	return &c
}

// writeRouteStatus rewrites this controller's entries for this Gateway in
// every route's status: results from res, and removal where a route no
// longer attaches.
//
// Entries for a ListenerSet belong to the Gateway the ListenerSet names. An
// entry of this controller for a ListenerSet that no Gateway of this
// controller serves any more (deleted, or moved to another Gateway) is
// removed by whichever Gateway's reconcile comes across it.
func (r *GatewayReconciler) writeRouteStatus(ctx context.Context, gw *gwv1.Gateway, rs *routeSet, res *translate.Result, out *outcome, sets listenerSets) error {
	results := map[translate.RouteRef]translate.RouteResult{}
	for _, rr := range res.Routes {
		results[rr.Ref] = rr
	}
	own := map[types.NamespacedName]bool{}
	for _, ls := range sets.of(gw) {
		own[types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}] = true
	}
	served := map[types.NamespacedName]bool{}
	servedElsewhere := func(key types.NamespacedName) bool {
		if v, ok := served[key]; ok {
			return v
		}
		v := false
		if ls := sets[key]; ls != nil {
			if parent, ok := listenerSetGateway(ls); ok {
				other := &gwv1.Gateway{}
				if err := r.Get(ctx, parent, other); err == nil {
					gc, err := r.managedClass(ctx, string(other.Spec.GatewayClassName))
					// When in doubt, leave the entry to that Gateway.
					v = err != nil || gc != nil
				} else {
					v = !apierrors.IsNotFound(err)
				}
			}
		}
		served[key] = v
		return v
	}
	var errs []error
	for _, t := range rs.targets() {
		ref := translate.RouteRef{Kind: t.kind, Namespace: t.obj.GetNamespace(), Name: t.obj.GetName()}
		result, has := results[ref]
		ours := func(p gwv1.RouteParentStatus) bool {
			if string(p.ControllerName) != r.Config.ControllerName {
				return false
			}
			if key, ok := listenerSetParentRef(p.ParentRef, ref.Namespace); ok {
				return own[key] || !servedElsewhere(key)
			}
			return translate.ParentTargets(p.ParentRef, ref.Namespace, gw.Namespace, gw.Name)
		}
		hadOurs := false
		var parents, old []gwv1.RouteParentStatus
		for _, p := range t.status.Parents {
			if ours(p) {
				hadOurs = true
				old = append(old, p)
				continue
			}
			parents = append(parents, p)
		}
		if !has && !hadOurs {
			continue
		}
		for _, pr := range result.Parents {
			newConds := pr.Conditions
			if c := dnsCondition(pr, out, t.obj.GetGeneration()); c != nil {
				newConds = append(append([]metav1.Condition(nil), newConds...), *c)
			}
			var conds []metav1.Condition
			for _, o := range old {
				if parentRefEqual(o.ParentRef, pr.ParentRef) {
					for _, c := range o.Conditions {
						if meta.FindStatusCondition(newConds, c.Type) != nil {
							conds = append(conds, c)
						}
					}
				}
			}
			for _, c := range newConds {
				meta.SetStatusCondition(&conds, c)
			}
			parents = append(parents, gwv1.RouteParentStatus{
				ParentRef: pr.ParentRef, ControllerName: gwv1.GatewayController(r.Config.ControllerName), Conditions: conds,
			})
		}
		if routeParentsEqual(t.status.Parents, parents) {
			continue
		}
		orig := t.obj.DeepCopyObject().(client.Object)
		t.status.Parents = parents
		if t.status.Parents == nil {
			t.status.Parents = []gwv1.RouteParentStatus{}
		}
		if err := r.Status().Patch(ctx, t.obj, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("%s status: %w", ref, err))
		}
	}
	return errors.Join(errs...)
}

func parentRefEqual(a, b gwv1.ParentReference) bool {
	str := func(v any) string {
		switch x := v.(type) {
		case *gwv1.Group:
			if x != nil {
				return string(*x)
			}
		case *gwv1.Kind:
			if x != nil {
				return string(*x)
			}
		case *gwv1.Namespace:
			if x != nil {
				return string(*x)
			}
		case *gwv1.SectionName:
			if x != nil {
				return string(*x)
			}
		case *gwv1.PortNumber:
			if x != nil {
				return fmt.Sprint(*x)
			}
		}
		return ""
	}
	return str(a.Group) == str(b.Group) && str(a.Kind) == str(b.Kind) && str(a.Namespace) == str(b.Namespace) &&
		a.Name == b.Name && str(a.SectionName) == str(b.SectionName) && str(a.Port) == str(b.Port)
}

func routeParentsEqual(a, b []gwv1.RouteParentStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ControllerName != b[i].ControllerName || !parentRefEqual(a[i].ParentRef, b[i].ParentRef) || !equalConditions(a[i].Conditions, b[i].Conditions) {
			return false
		}
	}
	return true
}
