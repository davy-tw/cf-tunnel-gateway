package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// maxTunnelNamePrefix bounds the readable part of a tunnel name.
const maxTunnelNamePrefix = 52

// TunnelName is the name of the tunnel the controller creates for a
// Gateway: "<instance>-<namespace>-<name>-<hash>", where the hash covers
// the cluster identity, namespace and name. The name marks ownership: an
// existing tunnel is adopted only when its name matches exactly.
func TunnelName(instance, clusterID, namespace, name string) string {
	base := instance + "-" + namespace + "-" + name
	if len(base) > maxTunnelNamePrefix {
		base = base[:maxTunnelNamePrefix]
	}
	return base + "-" + hashString(clusterID + "/" + namespace + "/" + name)[:10]
}

func (r *GatewayReconciler) tunnelName(gw *gwv1.Gateway) string {
	return TunnelName(r.Config.Instance, r.ClusterID, gw.Namespace, gw.Name)
}

// getTunnel returns a tunnel, or nil when it does not exist or is deleted.
func (r *GatewayReconciler) getTunnel(ctx context.Context, creds *Credentials, id string) (*cloudflare.Tunnel, error) {
	t, err := r.CF.GetTunnel(ctx, creds.AccountID, creds.AccountToken, id)
	if cloudflare.IsNotFound(err) || err == nil && t.Deleted() {
		return nil, nil
	}
	return t, err
}

// ensureTunnel returns the Gateway's tunnel, creating it if needed, and
// the tunnel being replaced, if any.
func (r *GatewayReconciler) ensureTunnel(ctx context.Context, gw *gwv1.Gateway, creds *Credentials) (*cloudflare.Tunnel, string, error) {
	recorded := gw.Annotations[config.AnnotationTunnel]
	bound := gw.Annotations[config.AnnotationTunnelID]
	previous := gw.Annotations[config.AnnotationPreviousTunnel]
	replace := func(current string) (string, error) {
		// current is being replaced; a tunnel replaced earlier that is
		// still around is released now.
		if recorded == "" || recorded == current {
			if previous == current {
				return "", nil
			}
			return previous, nil
		}
		if previous != "" && previous != recorded && previous != current && gw.Annotations[config.AnnotationKeepTunnel] != "true" {
			if err := r.deleteTunnel(ctx, creds, previous); err != nil {
				return "", err
			}
		}
		return recorded, nil
	}

	if bound != "" {
		t, err := r.getTunnel(ctx, creds, bound)
		if err != nil {
			return nil, "", err
		}
		if t == nil {
			return nil, "", &invalidParamsError{fmt.Sprintf("tunnel %s (annotation %s) does not exist in account %s", bound, config.AnnotationTunnelID, creds.AccountID)}
		}
		if t.ConfigSrc != "" && t.ConfigSrc != "cloudflare" {
			return nil, "", &invalidParamsError{fmt.Sprintf("tunnel %s is locally configured; only remotely-managed tunnels can be bound", bound)}
		}
		if other, err := r.boundElsewhere(ctx, gw, bound); err != nil {
			return nil, "", err
		} else if other != "" {
			return nil, "", &invalidParamsError{fmt.Sprintf("tunnel %s is already used by Gateway %s", bound, other)}
		}
		prev, err := replace(bound)
		return t, prev, err
	}

	name := r.tunnelName(gw)
	if recorded != "" {
		t, err := r.getTunnel(ctx, creds, recorded)
		if err != nil {
			return nil, "", err
		}
		if t != nil && t.Name == name {
			prev, err := replace(recorded)
			return t, prev, err
		}
	}
	existing, err := r.CF.ListTunnels(ctx, creds.AccountID, creds.AccountToken, name)
	if err != nil {
		return nil, "", err
	}
	for i := range existing {
		if t := &existing[i]; t.Name == name && !t.Deleted() {
			prev, err := replace(t.ID)
			return t, prev, err
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", err
	}
	t, err := r.CF.CreateTunnel(ctx, creds.AccountID, creds.AccountToken, name, base64.StdEncoding.EncodeToString(secret))
	if err != nil {
		return nil, "", err
	}
	r.event(gw, corev1.EventTypeNormal, "TunnelCreated", "created Cloudflare tunnel %s (%s)", name, t.ID)
	// Record the ID at once, so that a failure later in this reconcile
	// does not create a second tunnel.
	if err := r.setAnnotations(ctx, gw, map[string]string{config.AnnotationTunnel: t.ID}); err != nil {
		return nil, "", err
	}
	prev, err := replace(t.ID)
	return t, prev, err
}

// boundElsewhere returns the Gateway, other than gw, that uses tunnel id.
func (r *GatewayReconciler) boundElsewhere(ctx context.Context, gw *gwv1.Gateway, id string) (string, error) {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return "", err
	}
	for _, other := range list.Items {
		if other.UID == gw.UID {
			continue
		}
		a := other.Annotations
		if a[config.AnnotationTunnel] == id || a[config.AnnotationPreviousTunnel] == id ||
			a[config.AnnotationTunnelID] == id && olderGateway(&other, gw) {
			return other.Namespace + "/" + other.Name, nil
		}
	}
	return "", nil
}

// writeConfig replaces the tunnel's remote configuration when it changed,
// and at least every refreshEvery.
func (r *GatewayReconciler) writeConfig(ctx context.Context, creds *Credentials, id string, cfg *cloudflare.TunnelConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	r.mu.Lock()
	e, ok := r.configs[id]
	r.mu.Unlock()
	if ok && bytes.Equal(e.data, data) && time.Since(e.at) < refreshEvery {
		return nil
	}
	if err := r.CF.PutTunnelConfig(ctx, creds.AccountID, creds.AccountToken, id, cfg); err != nil {
		return fmt.Errorf("writing tunnel configuration: %w", err)
	}
	r.mu.Lock()
	r.configs[id] = configEntry{data: data, at: time.Now()}
	r.mu.Unlock()
	return nil
}

// connectorToken returns the tunnel's connector token.
func (r *GatewayReconciler) connectorToken(ctx context.Context, creds *Credentials, id string) (string, error) {
	r.mu.Lock()
	tok, ok := r.tokens[id]
	r.mu.Unlock()
	if ok {
		return tok, nil
	}
	tok, err := r.CF.TunnelToken(ctx, creds.AccountID, creds.AccountToken, id)
	if err != nil {
		return "", fmt.Errorf("getting the connector token: %w", err)
	}
	r.mu.Lock()
	r.tokens[id] = tok
	r.mu.Unlock()
	return tok, nil
}

// deleteTunnel deletes a tunnel so that its connectors cannot come back:
// the tunnel secret is replaced first (invalidating every connector
// token), then the open connections are dropped, then the tunnel goes.
func (r *GatewayReconciler) deleteTunnel(ctx context.Context, creds *Credentials, id string) error {
	t, err := r.getTunnel(ctx, creds, id)
	if err != nil || t == nil {
		r.forget(id)
		return err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	if err := r.CF.RotateTunnelSecret(ctx, creds.AccountID, creds.AccountToken, id, base64.StdEncoding.EncodeToString(secret)); err != nil && !cloudflare.IsNotFound(err) {
		return fmt.Errorf("rotating the secret of tunnel %s: %w", id, err)
	}
	if err := r.CF.DeleteTunnelConnections(ctx, creds.AccountID, creds.AccountToken, id); err != nil && !cloudflare.IsNotFound(err) {
		return fmt.Errorf("dropping the connections of tunnel %s: %w", id, err)
	}
	if err := r.CF.DeleteTunnel(ctx, creds.AccountID, creds.AccountToken, id); err != nil && !cloudflare.IsNotFound(err) {
		return fmt.Errorf("deleting tunnel %s: %w", id, err)
	}
	r.forget(id)
	return nil
}

func (r *GatewayReconciler) forget(id string) {
	r.mu.Lock()
	delete(r.configs, id)
	delete(r.tokens, id)
	r.mu.Unlock()
}

// ---- Access applications ----

func (r *GatewayReconciler) accessPrefix(gw *gwv1.Gateway) string {
	return fmt.Sprintf("cf-tunnel-gateway:%s:%s:", r.Config.DNSOwnerID, gatewayKey(gw))
}

// syncAccess keeps a deny-all self-hosted Access application per TCP
// hostname, when Access applications are turned on.
func (r *GatewayReconciler) syncAccess(ctx context.Context, gw *gwv1.Gateway, creds *Credentials, hosts []translate.Hostname) error {
	if !r.Config.AccessApps {
		return nil
	}
	apps, err := r.CF.ListAccessApps(ctx, creds.AccountID, creds.AccountToken)
	if err != nil {
		return fmt.Errorf("listing Access applications: %w", err)
	}
	prefix := r.accessPrefix(gw)
	have := map[string]bool{}
	for _, a := range apps {
		have[a.Name] = true
	}
	keep := map[string]bool{}
	for _, h := range hosts {
		if !h.TCP {
			continue
		}
		name := prefix + h.Name
		keep[name] = true
		if have[name] {
			continue
		}
		if _, err := r.CF.CreateAccessApp(ctx, creds.AccountID, creds.AccountToken, cloudflare.AccessApp{
			Name: name, Domain: h.Name, Type: "self_hosted", SessionDuration: "24h",
		}); err != nil {
			return fmt.Errorf("creating the Access application for %s: %w", h.Name, err)
		}
		r.event(gw, corev1.EventTypeNormal, "AccessApplicationCreated",
			"created Access application %q for %s; it denies everyone until you add a policy", name, h.Name)
	}
	return r.deleteAccessApps(ctx, gw, creds, keep)
}

// deleteAccessApps deletes the Gateway's Access applications not in keep.
func (r *GatewayReconciler) deleteAccessApps(ctx context.Context, gw *gwv1.Gateway, creds *Credentials, keep map[string]bool) error {
	if !r.Config.AccessApps {
		return nil
	}
	apps, err := r.CF.ListAccessApps(ctx, creds.AccountID, creds.AccountToken)
	if err != nil {
		return fmt.Errorf("listing Access applications: %w", err)
	}
	prefix := r.accessPrefix(gw)
	for _, a := range apps {
		if len(a.Name) > len(prefix) && a.Name[:len(prefix)] == prefix && !keep[a.Name] {
			if err := r.CF.DeleteAccessApp(ctx, creds.AccountID, creds.AccountToken, a.ID); err != nil {
				return fmt.Errorf("deleting Access application %q: %w", a.Name, err)
			}
		}
	}
	return nil
}
