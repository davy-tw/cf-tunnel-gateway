package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

var invalidLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// DefaultLabel is the DNS label derived for an object: "<name>-<namespace>"
// or "<name>", lower-cased, with invalid characters replaced, and
// truncated with a hash at 63 characters.
func DefaultLabel(template, name, namespace, suffix string) string {
	s := name
	if template != config.TemplateName {
		s += "-" + namespace
	}
	if suffix != "" {
		s += "-" + suffix
	}
	full := s
	s = strings.Trim(invalidLabelChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:54], "-") + "-" + hashString(full)[:8]
	}
	return s
}

// warnedBases remembers base domains already warned about.
var warnedBases sync.Map

// baseDomain returns the base domain for derived hostnames of src, or ""
// and why there is none.
func (g *generator) baseDomain(ctx context.Context, src sourceObj) (string, string, error) {
	pol, err := namespacePolicy(ctx, g.Client, g.Config, src.obj.GetNamespace())
	if err != nil {
		return "", "", err
	}
	base, fromNS := g.Config.BaseDomain, false
	if pol.BaseDomain != "" {
		base, fromNS = pol.BaseDomain, true
	}
	if base == "" {
		return "", "no default base domain is configured", nil
	}
	gc, err := g.managedClass(ctx, generatedClass(g.Config, pol))
	if err != nil {
		return "", "", err
	}
	if gc == nil {
		if fromNS {
			return "", fmt.Sprintf("the base domain %s of namespace %s cannot be checked: GatewayClass %q is not served by this controller", base, src.obj.GetNamespace(), generatedClass(g.Config, pol)), nil
		}
		return base, "", nil
	}
	creds, err := g.credentials(ctx, gc)
	if err != nil {
		var ip *invalidParamsError
		if asInvalid(err, &ip) {
			if fromNS {
				return "", "the namespace base domain cannot be checked: " + ip.msg, nil
			}
			return base, "", nil
		}
		return "", "", err
	}
	zone, err := findZone(ctx, g.CF, creds.DNSToken, "", base)
	if err != nil {
		return "", "", err
	}
	if zone == nil {
		if fromNS {
			return "", fmt.Sprintf("the base domain %s (annotation %s on namespace %s) is not in a zone GatewayClass %s can see", base, config.AnnotationBaseDomain, src.obj.GetNamespace(), gc.Name), nil
		}
		return base, "", nil
	}
	if !strings.EqualFold(zone.Name, base) {
		if _, warned := warnedBases.LoadOrStore(base, true); !warned {
			g.event(src.obj, corev1.EventTypeNormal, "AdvancedCertificateNeeded",
				"base domain %s is below the zone apex %s: hostnames under it need an advanced certificate in Cloudflare (Universal SSL covers one level only)", base, zone.Name)
		}
	}
	return base, "", nil
}

// derivedHostname returns the default hostname of src (with an optional
// label suffix), or "" and why there is none.
func (g *generator) derivedHostname(ctx context.Context, src sourceObj, suffix string) (string, string, error) {
	if src.obj.GetAnnotations()[config.AnnotationDefaultHostname] == "false" {
		return "", fmt.Sprintf("default hostnames are turned off (annotation %s)", config.AnnotationDefaultHostname), nil
	}
	base, why, err := g.baseDomain(ctx, src)
	if err != nil || base == "" {
		return "", why, err
	}
	host := DefaultLabel(g.Config.HostnameTemplate, src.obj.GetName(), src.obj.GetNamespace(), suffix) + "." + base
	if other, err := g.hostnameTaken(ctx, src, host); err != nil {
		return "", "", err
	} else if other != "" {
		return "", fmt.Sprintf("the default hostname %s is already used by %s", host, other), nil
	}
	return host, "", nil
}

// hostnameTaken returns the route that already publishes host for another
// object and keeps it: an explicitly written hostname always does, a
// derived one when its route is older than src.
func (g *generator) hostnameTaken(ctx context.Context, src sourceObj, host string) (string, error) {
	mine := func(o client.Object) bool {
		l := o.GetLabels()
		return o.GetNamespace() == src.obj.GetNamespace() && l[config.LabelSourceKind] == src.label && l[config.LabelSourceName] == LabelValue(src.obj.GetName())
	}
	derived := func(o client.Object) bool {
		for _, h := range strings.Split(o.GetAnnotations()[config.AnnotationDefaultHostname], ",") {
			if strings.TrimSpace(h) == host {
				return true
			}
		}
		return false
	}
	keeps := func(o client.Object) bool {
		if !derived(o) {
			return true
		}
		ct := o.GetCreationTimestamp()
		sct := src.obj.GetCreationTimestamp()
		return ct.Before(&sct)
	}
	var hl gwv1.HTTPRouteList
	if err := g.List(ctx, &hl); err != nil {
		return "", err
	}
	for i := range hl.Items {
		r := &hl.Items[i]
		if mine(r) {
			continue
		}
		for _, h := range r.Spec.Hostnames {
			if strings.EqualFold(string(h), host) && keeps(r) {
				return "HTTPRoute " + r.Namespace + "/" + r.Name, nil
			}
		}
	}
	if g.Features.TCPRoute() {
		var tl gwv1.TCPRouteList
		if err := g.List(ctx, &tl); err != nil {
			return "", err
		}
		for i := range tl.Items {
			r := &tl.Items[i]
			if !mine(r) && strings.EqualFold(r.Annotations[config.AnnotationHostname], host) && keeps(r) {
				return "TCPRoute " + r.Namespace + "/" + r.Name, nil
			}
		}
	}
	return "", nil
}
