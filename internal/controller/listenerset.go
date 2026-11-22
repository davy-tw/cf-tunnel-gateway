package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// reasonParentNotProgrammed is the ListenerSet Programmed reason the
// Gateway API documents for a parent Gateway that is not programmed.
const reasonParentNotProgrammed = "ParentNotProgrammed"

// listenerSets holds every ListenerSet in the cluster.
type listenerSets map[types.NamespacedName]*gwv1.ListenerSet

func (r *GatewayReconciler) listListenerSets(ctx context.Context) (listenerSets, error) {
	out := listenerSets{}
	if !r.Features.ListenerSet() {
		return out, nil
	}
	var l gwv1.ListenerSetList
	if err := r.List(ctx, &l); err != nil {
		return nil, err
	}
	for i := range l.Items {
		ls := &l.Items[i]
		out[types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}] = ls
	}
	return out, nil
}

// of returns the ListenerSets whose parentRef names gw, sorted by name.
func (s listenerSets) of(gw *gwv1.Gateway) []*gwv1.ListenerSet {
	var out []*gwv1.ListenerSet
	for _, ls := range s {
		if translate.ListenerSetTargets(ls, gw.Namespace, gw.Name) {
			out = append(out, ls)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out
}

// listenerSetParentRef returns the ListenerSet a parentRef (from an object
// in routeNS) names, if it names one.
func listenerSetParentRef(p gwv1.ParentReference, routeNS string) (types.NamespacedName, bool) {
	if p.Group != nil && string(*p.Group) != gwv1.GroupName || p.Kind == nil || string(*p.Kind) != translate.KindListenerSet {
		return types.NamespacedName{}, false
	}
	ns := routeNS
	if p.Namespace != nil && *p.Namespace != "" {
		ns = string(*p.Namespace)
	}
	return types.NamespacedName{Namespace: ns, Name: string(p.Name)}, true
}

// listenerSetGateway returns the Gateway a ListenerSet's parentRef names.
func listenerSetGateway(ls *gwv1.ListenerSet) (types.NamespacedName, bool) {
	p := ls.Spec.ParentRef
	if p.Group != nil && string(*p.Group) != gwv1.GroupName || p.Kind != nil && string(*p.Kind) != translate.KindGateway {
		return types.NamespacedName{}, false
	}
	ns := ls.Namespace
	if p.Namespace != nil && *p.Namespace != "" {
		ns = string(*p.Namespace)
	}
	return types.NamespacedName{Namespace: ns, Name: string(p.Name)}, true
}

// gatewayForListenerSet maps a ListenerSet to the Gateway it names. On an
// update both the old and the new object are mapped, so a moved
// ListenerSet reaches both Gateways.
func (r *GatewayReconciler) gatewayForListenerSet(_ context.Context, obj client.Object) []reconcile.Request {
	ls, ok := obj.(*gwv1.ListenerSet)
	if !ok {
		return nil
	}
	key, ok := listenerSetGateway(ls)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: key}}
}

// writeListenerSetStatus writes the status of the ListenerSets that name
// gw, once the Gateway's own status is written: Programmed follows the
// Gateway's.
func (r *GatewayReconciler) writeListenerSetStatus(ctx context.Context, gw *gwv1.Gateway, sets []*gwv1.ListenerSet, res *translate.Result, ip *invalidParamsError) error {
	results := map[types.NamespacedName]translate.ListenerSetResult{}
	for _, lr := range res.ListenerSets {
		results[lr.Ref] = lr
	}
	gp := meta.FindStatusCondition(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed))
	var errs []error
	for _, ls := range sets {
		lr, ok := results[types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}]
		if !ok {
			continue
		}
		gen := ls.Generation
		accepted, reason, msg := lr.Accepted, lr.Reason, lr.Message
		if ip != nil && reason != string(gwv1.ListenerSetReasonNotAllowed) {
			accepted, reason = false, string(gwv1.ListenerSetReasonParentNotAccepted)
			msg = fmt.Sprintf("Gateway %s/%s is not accepted: %s", gw.Namespace, gw.Name, ip.msg)
		}
		ac := metav1.Condition{Type: string(gwv1.ListenerSetConditionAccepted), Status: metav1.ConditionTrue,
			Reason: reason, Message: msg, ObservedGeneration: gen}
		pc := metav1.Condition{Type: string(gwv1.ListenerSetConditionProgrammed), Status: metav1.ConditionFalse,
			Reason: reason, Message: msg, ObservedGeneration: gen}
		switch {
		case !accepted:
			ac.Status = metav1.ConditionFalse
			if reason == string(gwv1.ListenerSetReasonParentNotAccepted) {
				pc.Reason = reasonParentNotProgrammed
			}
		case gp != nil && gp.Status == metav1.ConditionTrue:
			pc.Status, pc.Reason, pc.Message = metav1.ConditionTrue, string(gwv1.ListenerSetReasonProgrammed), gp.Message
		default:
			pc.Reason = reasonParentNotProgrammed
			pc.Message = "the parent Gateway is not programmed"
			if gp != nil {
				pc.Message += ": " + gp.Message
			}
		}
		// Listener Programmed follows the ListenerSet's.
		lp := metav1.Condition{Status: pc.Status, Message: pc.Message}
		orig := ls.DeepCopy()
		meta.SetStatusCondition(&ls.Status.Conditions, ac)
		meta.SetStatusCondition(&ls.Status.Conditions, pc)
		var listeners []gwv1.ListenerEntryStatus
		for _, l := range lr.Listeners {
			var prev []metav1.Condition
			for _, old := range orig.Status.Listeners {
				if old.Name == l.Name {
					prev = old.Conditions
				}
			}
			conds, kinds := listenerConditions(prev, l, lp, gen)
			listeners = append(listeners, gwv1.ListenerEntryStatus{Name: l.Name, SupportedKinds: kinds, AttachedRoutes: l.AttachedRoutes, Conditions: conds})
		}
		ls.Status.Listeners = listeners
		if listenerSetStatusEqual(&orig.Status, &ls.Status) {
			continue
		}
		if err := r.Status().Patch(ctx, ls, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("ListenerSet %s/%s status: %w", ls.Namespace, ls.Name, err))
		}
	}
	return errors.Join(errs...)
}

// listenerConditions merges a listener's translated conditions into its
// previous ones and adds Programmed, which follows programmed (the
// Gateway's or the ListenerSet's). It also returns the supported kinds,
// never nil.
func listenerConditions(prev []metav1.Condition, lr translate.ListenerResult, programmed metav1.Condition, gen int64) ([]metav1.Condition, []gwv1.RouteGroupKind) {
	var conds []metav1.Condition
	for _, c := range prev {
		if meta.FindStatusCondition(lr.Conditions, c.Type) != nil || c.Type == string(gwv1.ListenerConditionProgrammed) {
			conds = append(conds, c)
		}
	}
	for _, c := range lr.Conditions {
		meta.SetStatusCondition(&conds, c)
	}
	lp := metav1.Condition{Type: string(gwv1.ListenerConditionProgrammed), ObservedGeneration: gen,
		Status: programmed.Status, Reason: string(gwv1.ListenerReasonProgrammed), Message: programmed.Message}
	switch {
	case !lr.Valid:
		lp.Status, lp.Reason, lp.Message = metav1.ConditionFalse, lr.InvalidReason, "listener is not valid"
		if lp.Reason == "" {
			lp.Reason = string(gwv1.ListenerReasonInvalid)
		}
	case programmed.Status != metav1.ConditionTrue:
		lp.Status = metav1.ConditionFalse
		lp.Reason = string(gwv1.ListenerReasonPending)
	}
	meta.SetStatusCondition(&conds, lp)
	kinds := lr.SupportedKinds
	if kinds == nil {
		kinds = []gwv1.RouteGroupKind{}
	}
	return conds, kinds
}

// releaseListenerSets hands back the ListenerSets that name gw once this
// controller stops serving gw: on deletion they get ParentNotAccepted; when
// gw moved to another controller's class, they go back to the "waiting for
// controller" conditions the CRD starts with, for that controller to
// replace.
func (r *GatewayReconciler) releaseListenerSets(ctx context.Context, gw *gwv1.Gateway, deleting bool) error {
	if !r.Features.ListenerSet() {
		return nil
	}
	if deleting {
		return r.notAcceptListenerSets(ctx, gw, "the parent Gateway is being deleted")
	}
	sets, err := r.listListenerSets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ls := range sets.of(gw) {
		errs = append(errs, r.releaseListenerSet(ctx, ls, metav1.ConditionUnknown, string(gwv1.ListenerSetReasonPending),
			string(gwv1.ListenerSetReasonPending), "Waiting for controller"))
	}
	return errors.Join(errs...)
}

// notAcceptListenerSets marks the ListenerSets that name gw as not
// accepted while gw cannot serve them (it is deleted, not allowed, or its
// class is gone).
func (r *GatewayReconciler) notAcceptListenerSets(ctx context.Context, gw *gwv1.Gateway, why string) error {
	sets, err := r.listListenerSets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, ls := range sets.of(gw) {
		errs = append(errs, r.releaseListenerSet(ctx, ls, metav1.ConditionFalse,
			string(gwv1.ListenerSetReasonParentNotAccepted), reasonParentNotProgrammed, why))
	}
	return errors.Join(errs...)
}

// releaseListenerSet sets a ListenerSet's Accepted and Programmed
// conditions and drops its listener status.
func (r *GatewayReconciler) releaseListenerSet(ctx context.Context, ls *gwv1.ListenerSet, status metav1.ConditionStatus, acceptedReason, programmedReason, why string) error {
	orig := ls.DeepCopy()
	for _, c := range []metav1.Condition{
		{Type: string(gwv1.ListenerSetConditionAccepted), Reason: acceptedReason},
		{Type: string(gwv1.ListenerSetConditionProgrammed), Reason: programmedReason},
	} {
		c.Status, c.Message, c.ObservedGeneration = status, why, ls.Generation
		meta.SetStatusCondition(&ls.Status.Conditions, c)
	}
	ls.Status.Listeners = nil
	if listenerSetStatusEqual(&orig.Status, &ls.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.Status().Patch(ctx, ls, client.MergeFrom(orig)))
}

// releaseMovedListenerSets hands back ListenerSets whose status gw wrote
// but whose parentRef now names a Gateway that no Gateway of this
// controller serves: one that does not exist gets ParentNotAccepted,
// another controller's gets the "waiting for controller" conditions the
// CRD starts with. Status that is already about the ListenerSet's current
// generation was written after the move, by whoever serves it now, and is
// left alone. The ListenerSets gw serves are then recorded for gw.
func (r *GatewayReconciler) releaseMovedListenerSets(ctx context.Context, gw *gwv1.Gateway, sets listenerSets) error {
	var current []string
	for _, ls := range sets.of(gw) {
		current = append(current, ls.Namespace+"/"+ls.Name)
	}
	rec, err := r.recorded(ctx, gw)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range splitList(rec[config.AnnotationListenerSets]) {
		ns, name, ok := strings.Cut(k, "/")
		if !ok || slices.Contains(current, k) {
			continue
		}
		ls := sets[types.NamespacedName{Namespace: ns, Name: name}]
		if ls == nil {
			continue // deleted
		}
		if c := meta.FindStatusCondition(ls.Status.Conditions, string(gwv1.ListenerSetConditionAccepted)); c == nil || c.ObservedGeneration >= ls.Generation {
			continue
		}
		parent, ok := listenerSetGateway(ls)
		if !ok {
			errs = append(errs, r.releaseListenerSet(ctx, ls, metav1.ConditionUnknown, string(gwv1.ListenerSetReasonPending),
				string(gwv1.ListenerSetReasonPending), "Waiting for controller"))
			continue
		}
		other := &gwv1.Gateway{}
		switch err := r.Get(ctx, parent, other); {
		case apierrors.IsNotFound(err):
			errs = append(errs, r.releaseListenerSet(ctx, ls, metav1.ConditionFalse, string(gwv1.ListenerSetReasonParentNotAccepted),
				reasonParentNotProgrammed, fmt.Sprintf("Gateway %s not found", parent)))
		case err != nil:
			errs = append(errs, err)
		default:
			gc, err := r.managedClass(ctx, string(other.Spec.GatewayClassName))
			switch {
			case err != nil:
				errs = append(errs, err)
			case gc == nil:
				errs = append(errs, r.releaseListenerSet(ctx, ls, metav1.ConditionUnknown, string(gwv1.ListenerSetReasonPending),
					string(gwv1.ListenerSetReasonPending), "Waiting for controller"))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err // keep the record and retry
	}
	return r.setAnnotations(ctx, gw, map[string]string{config.AnnotationListenerSets: strings.Join(current, ",")})
}

func listenerSetStatusEqual(a, b *gwv1.ListenerSetStatus) bool {
	if !equalConditions(a.Conditions, b.Conditions) || len(a.Listeners) != len(b.Listeners) {
		return false
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
