package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// nsPolicy is a namespace's policy annotations.
type nsPolicy struct {
	// AllowedClasses is nil when every class is allowed.
	AllowedClasses []string
	// GatewayClass overrides the class of generated Gateways.
	GatewayClass string
	// AllowedHostnames is nil when unrestricted; an empty non-nil slice
	// allows nothing.
	AllowedHostnames []string
	BaseDomain       string
	// AllowTunnelID lets the namespace's Gateways bind existing tunnels
	// with the tunnel-id annotation.
	AllowTunnelID bool
}

func (p nsPolicy) classAllowed(name string) bool {
	if p.AllowedClasses == nil {
		return true
	}
	for _, c := range p.AllowedClasses {
		if c == name {
			return true
		}
	}
	return false
}

// namespacePolicy reads a namespace's policy annotations.
func namespacePolicy(ctx context.Context, c client.Reader, cfg *config.Config, ns string) (nsPolicy, error) {
	n := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: ns}, n); err != nil && !apierrors.IsNotFound(err) {
		return nsPolicy{}, err
	}
	return policyFromNamespace(cfg, n), nil
}

func policyFromNamespace(cfg *config.Config, n *corev1.Namespace) nsPolicy {
	a := n.Annotations
	var p nsPolicy
	if v, ok := a[config.AnnotationAllowedGatewayClasses]; ok {
		p.AllowedClasses = []string{}
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" {
				p.AllowedClasses = append(p.AllowedClasses, c)
			}
		}
	}
	p.GatewayClass = strings.TrimSpace(a[config.AnnotationGatewayClass])
	p.AllowTunnelID = strings.TrimSpace(a[config.AnnotationAllowTunnelID]) == "true"
	if v, ok := a[config.AnnotationAllowedHostnames]; ok {
		p.AllowedHostnames = translate.ParsePatterns(v)
	} else if cfg.DenyUnlistedHostnames {
		p.AllowedHostnames = []string{}
	}
	p.BaseDomain = strings.ToLower(strings.Trim(strings.TrimSpace(a[config.AnnotationBaseDomain]), "."))
	return p
}

// allowedHostnamesFunc adapts namespace policies for translate.Input.
func allowedHostnamesFunc(ctx context.Context, c client.Reader, cfg *config.Config) func(string) []string {
	cache := map[string][]string{}
	return func(ns string) []string {
		if v, ok := cache[ns]; ok {
			return v
		}
		p, err := namespacePolicy(ctx, c, cfg, ns)
		if err != nil {
			// Fail closed: a namespace whose policy cannot be read
			// publishes nothing until the next reconcile.
			p.AllowedHostnames = []string{}
		}
		cache[ns] = p.AllowedHostnames
		return p.AllowedHostnames
	}
}

// generatedClass returns the class for Gateways generated in a namespace.
func generatedClass(cfg *config.Config, p nsPolicy) string {
	if p.GatewayClass != "" {
		return p.GatewayClass
	}
	return cfg.GatewayClass
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}
