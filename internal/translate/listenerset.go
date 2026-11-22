package translate

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// OwnerGateway is the owner of the hostnames the Gateway's own listeners
// and routes use (Input.HostOwners).
const OwnerGateway = "Gateway"

// ListenerSetOwner is the owner key of a ListenerSet's hostnames. It
// carries the UID, so a ListenerSet that is deleted and created again is
// a new owner.
func ListenerSetOwner(ls *gwv1.ListenerSet) string {
	return fmt.Sprintf("%s %s/%s %s", KindListenerSet, ls.Namespace, ls.Name, ls.UID)
}

// ListenerSetResult is the status of one ListenerSet whose parentRef names
// the Gateway.
type ListenerSetResult struct {
	Ref        types.NamespacedName
	Generation int64
	// Accepted ListenerSets are attached to the Gateway: they are allowed
	// and have at least one valid listener.
	Accepted bool
	// Reason and Message describe the Accepted condition.
	Reason  string
	Message string
	// Listeners is empty when the Gateway does not allow the ListenerSet.
	Listeners []ListenerResult
}

type setState struct {
	ls        *gwv1.ListenerSet
	allowed   bool
	accepted  bool
	listeners []*listenerState
	// rank orders allowed ListenerSets after the Gateway (from 1).
	rank int
}

// ListenerSetTargets reports whether a ListenerSet's parentRef names the
// Gateway namespace/name.
func ListenerSetTargets(ls *gwv1.ListenerSet, ns, name string) bool {
	p := ls.Spec.ParentRef
	if p.Group != nil && string(*p.Group) != gwv1.GroupName {
		return false
	}
	if p.Kind != nil && string(*p.Kind) != KindGateway {
		return false
	}
	pns := ls.Namespace
	if p.Namespace != nil && *p.Namespace != "" {
		pns = string(*p.Namespace)
	}
	return pns == ns && string(p.Name) == name
}

// listenerSetAllowed applies the Gateway's allowedListeners. Without it,
// no ListenerSet may attach.
func (t *translator) listenerSetAllowed(ls *gwv1.ListenerSet) bool {
	gw := t.in.Gateway
	from := gwv1.NamespacesFromNone
	var sel *metav1.LabelSelector
	if al := gw.Spec.AllowedListeners; al != nil && al.Namespaces != nil {
		if al.Namespaces.From != nil {
			from = *al.Namespaces.From
		}
		sel = al.Namespaces.Selector
	}
	switch from {
	case gwv1.NamespacesFromAll:
		return true
	case gwv1.NamespacesFromSame:
		return ls.Namespace == gw.Namespace
	case gwv1.NamespacesFromSelector:
		if sel == nil {
			return false
		}
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return false
		}
		return s.Matches(labels.Set(t.in.NamespaceLabels[ls.Namespace]))
	}
	return false
}

func entryListener(e gwv1.ListenerEntry) gwv1.Listener {
	return gwv1.Listener{Name: e.Name, Hostname: e.Hostname, Port: e.Port, Protocol: e.Protocol, TLS: e.TLS, AllowedRoutes: e.AllowedRoutes}
}

// prepareListenerSets orders the ListenerSets that name the Gateway in the
// Gateway API precedence order (creation time, then namespace/name) and
// ranks the allowed ones after the Gateway. Their listeners are built
// later, owner by owner (buildSetListeners).
func (t *translator) prepareListenerSets() {
	var sets []*gwv1.ListenerSet
	for _, ls := range t.in.ListenerSets {
		if ListenerSetTargets(ls, t.in.Gateway.Namespace, t.in.Gateway.Name) {
			sets = append(sets, ls)
		}
	}
	sort.SliceStable(sets, func(i, j int) bool {
		a, b := sets[i], sets[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	// The Gateway's listeners keep their ports: those were declared by
	// the Gateway's owner.
	for _, ls := range t.listeners {
		if ls.accepted {
			t.earlier = append(t.earlier, ls)
		}
	}
	rank := 0
	for _, ls := range sets {
		ss := &setState{ls: ls, allowed: t.listenerSetAllowed(ls)}
		t.sets[types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}] = ss
		t.setOrder = append(t.setOrder, ss)
		if ss.allowed {
			rank++
			ss.rank = rank
		}
	}
}

// rankOf returns the rank of a recorded owner key, or -1 when the owner
// no longer holds hostnames here (a ListenerSet that is gone, was
// recreated, moved to another Gateway or is no longer allowed).
func (t *translator) rankOf(key string) int {
	if key == OwnerGateway {
		return 0
	}
	for _, ss := range t.setOrder {
		if ss.allowed && ListenerSetOwner(ss.ls) == key {
			return ss.rank
		}
	}
	return -1
}

func (t *translator) setOfRank(rank int) *setState {
	for _, ss := range t.setOrder {
		if ss.allowed && ss.rank == rank {
			return ss
		}
	}
	return nil
}

// ownerKey is the recorded owner key of a rank.
func (t *translator) ownerKey(rank int) string {
	if ss := t.setOfRank(rank); rank > 0 && ss != nil {
		return ListenerSetOwner(ss.ls)
	}
	return OwnerGateway
}

// ownerName describes the owner of a rank.
func (t *translator) ownerName(rank int) string {
	if ss := t.setOfRank(rank); rank > 0 && ss != nil {
		return fmt.Sprintf("ListenerSet %s/%s", ss.ls.Namespace, ss.ls.Name)
	}
	return fmt.Sprintf("Gateway %s/%s", t.in.Gateway.Namespace, t.in.Gateway.Name)
}

// seedOwners gives the hostnames recorded by earlier reconciles back to
// their owners, unless a listener of the Gateway names them.
func (t *translator) seedOwners() {
	hosts := make([]string, 0, len(t.in.HostOwners))
	for h := range t.in.HostOwners {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		rank := t.rankOf(t.in.HostOwners[h])
		if _, held := t.owner[h]; rank >= 0 && !held {
			t.owner[h] = rank
		}
	}
}

// claim takes a hostname for the owner of rank, or says why it cannot: the
// hostname belongs to another owner, or falls under a wildcard hostname
// a ListenerSet holds.
func (t *translator) claim(host string, rank int) string {
	if o, ok := t.owner[host]; ok {
		if o == rank {
			return ""
		}
		return fmt.Sprintf("hostname %s belongs to %s", host, t.ownerName(o))
	}
	var wildcards []string
	for h, o := range t.owner {
		if o != rank && o != 0 && strings.HasPrefix(h, "*.") {
			wildcards = append(wildcards, h)
		}
	}
	sort.Strings(wildcards)
	for _, w := range wildcards {
		if HostnameAllowed(host, []string{w}) {
			return fmt.Sprintf("hostname %s is covered by %s, which belongs to %s", host, w, t.ownerName(t.owner[w]))
		}
	}
	t.owner[host] = rank
	return ""
}

// conflictWith returns the conflict reason and message when l cannot be
// served next to an earlier listener: another protocol on the same port,
// or the same protocol and hostname on the same port.
func conflictWith(l gwv1.Listener, earlier []*listenerState) (gwv1.ListenerConditionReason, string) {
	for _, e := range earlier {
		if e.l.Port != l.Port {
			continue
		}
		if e.l.Protocol != l.Protocol {
			return gwv1.ListenerReasonProtocolConflict, fmt.Sprintf("port %d is already used with protocol %s by %s", l.Port, e.l.Protocol, e.describe())
		}
		if listenerHostname(e.l.Hostname) == listenerHostname(l.Hostname) {
			return gwv1.ListenerReasonHostnameConflict, fmt.Sprintf("port %d and hostname %q are already used by %s", l.Port, listenerHostname(l.Hostname), e.describe())
		}
	}
	return "", ""
}

func (ls *listenerState) describe() string {
	if ls.set == nil {
		return fmt.Sprintf("Gateway listener %q", ls.l.Name)
	}
	return fmt.Sprintf("ListenerSet %s/%s listener %q", ls.set.ls.Namespace, ls.set.ls.Name, ls.l.Name)
}

// buildSetListeners adds an allowed ListenerSet's listeners. A listener is
// not accepted when its namespace may not publish its hostname, when it
// conflicts with an earlier listener on the same port, or when its
// hostname belongs to another owner of the Gateway (on any port: Cloudflare
// serves a hostname on all its ports at once).
func (t *translator) buildSetListeners(ss *setState) {
	ls, rank, gen := ss.ls, ss.rank, ss.ls.Generation
	for _, e := range ls.Spec.Listeners {
		l := entryListener(e)
		st := t.newListener(l, ls.Namespace, ss, gen)
		if st.accepted {
			h := listenerHostname(l.Hostname)
			if why := t.hostnameNotAllowed(h, ls.Namespace, false); h != "" && why != "" {
				st.accepted, st.res.Valid, st.res.InvalidReason = false, false, ReasonHostnameNotAllowed
				replaceCond(&st.res.Conditions, cond(gwv1.ListenerConditionAccepted, false, ReasonHostnameNotAllowed, why, gen))
			} else {
				reason, msg := conflictWith(l, t.earlier)
				if reason == "" && h != "" && st.res.Valid {
					if why := t.claim(h, rank); why != "" {
						reason, msg = gwv1.ListenerReasonHostnameConflict, why
					}
				}
				if reason != "" {
					st.res.Valid, st.res.InvalidReason = false, string(reason)
					replaceCond(&st.res.Conditions, cond(gwv1.ListenerConditionAccepted, false, reason, msg, gen))
					replaceCond(&st.res.Conditions, cond(gwv1.ListenerConditionConflicted, true, reason, msg, gen))
				} else {
					t.earlier = append(t.earlier, st)
					if h != "" && st.res.Valid {
						t.declared[h] = rank
					}
				}
			}
		}
		ss.listeners = append(ss.listeners, st)
		if st.res.Valid {
			ss.accepted = true
		}
		t.listeners = append(t.listeners, st)
	}
}

func replaceCond(conds *[]metav1.Condition, c metav1.Condition) {
	for i := range *conds {
		if (*conds)[i].Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// listenerSetResults builds the ListenerSet statuses once routes are
// attached. A ListenerSet of a Gateway that is not accepted is not
// accepted either.
func (t *translator) listenerSetResults(gatewayAccepted bool) ([]ListenerSetResult, int32) {
	var out []ListenerSetResult
	var attached int32
	for _, ss := range t.setOrder {
		r := ListenerSetResult{Ref: types.NamespacedName{Namespace: ss.ls.Namespace, Name: ss.ls.Name}, Generation: ss.ls.Generation}
		valid := 0
		for _, l := range ss.listeners {
			r.Listeners = append(r.Listeners, l.res)
			if l.res.Valid {
				valid++
			}
		}
		switch {
		case !ss.allowed:
			r.Reason = string(gwv1.ListenerSetReasonNotAllowed)
			r.Message = fmt.Sprintf("Gateway %s/%s does not allow ListenerSets from namespace %q (spec.allowedListeners)",
				t.in.Gateway.Namespace, t.in.Gateway.Name, ss.ls.Namespace)
		case !gatewayAccepted:
			r.Reason = string(gwv1.ListenerSetReasonParentNotAccepted)
			r.Message = fmt.Sprintf("Gateway %s/%s is not accepted", t.in.Gateway.Namespace, t.in.Gateway.Name)
		case valid == 0:
			r.Reason = string(gwv1.ListenerSetReasonListenersNotValid)
			r.Message = "no listener is valid"
		case valid < len(ss.listeners):
			r.Accepted = true
			r.Reason = string(gwv1.ListenerSetReasonListenersNotValid)
			r.Message = fmt.Sprintf("%d of %d listeners are not valid", len(ss.listeners)-valid, len(ss.listeners))
		default:
			r.Accepted = true
			r.Reason = string(gwv1.ListenerSetReasonAccepted)
			r.Message = "ListenerSet accepted"
		}
		if r.Accepted {
			attached++
		}
		out = append(out, r)
	}
	return out, attached
}

// hostOwners returns the owners to record: of every published hostname,
// of every hostname an accepted ListenerSet listener declares, and the
// recorded hostnames of allowed ListenerSets that are not accepted at the
// moment (so that a failed edit does not give their hostnames away).
func (t *translator) hostOwners(published []Hostname) map[string]string {
	out := map[string]string{}
	for _, h := range published {
		if r, ok := t.owner[h.Name]; ok {
			out[h.Name] = t.ownerKey(r)
		}
	}
	for h, r := range t.declared {
		out[h] = t.ownerKey(r)
	}
	for h, k := range t.in.HostOwners {
		if _, set := out[h]; set {
			continue
		}
		if r := t.rankOf(k); r > 0 && !t.setOfRank(r).accepted && t.owner[h] == r {
			out[h] = k
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
