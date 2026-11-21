package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// Keys of a GatewayClass credentials Secret.
const (
	KeyAccountID   = "accountId"
	KeyAPIToken    = "apiToken"
	KeyDNSAPIToken = "dnsApiToken"
)

// Credentials are what a GatewayClass uses against Cloudflare.
type Credentials struct {
	AccountID    string
	AccountToken string
	DNSToken     string
	// SecretVersion identifies the class Secret's content.
	SecretVersion string
}

// invalidParamsError marks a GatewayClass whose parameters are unusable.
type invalidParamsError struct{ msg string }

func (e *invalidParamsError) Error() string { return e.msg }

func asInvalid(err error, target **invalidParamsError) bool {
	ip, ok := err.(*invalidParamsError)
	if ok {
		*target = ip
	}
	return ok
}

// classResolver looks up managed GatewayClasses and their credentials.
type classResolver struct {
	client.Client
	Config *config.Config
	CF     *cloudflare.Client
	// APIReader reads from the API server, bypassing the cache. It is set
	// from the manager; when nil, the client is used.
	APIReader client.Reader
}

func (r *classResolver) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// lookupClass returns the GatewayClass if this controller serves it, and
// whether a class of that name exists at all.
func (r *classResolver) lookupClass(ctx context.Context, name string) (*gwv1.GatewayClass, bool, error) {
	gc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, gc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if string(gc.Spec.ControllerName) != r.Config.ControllerName {
		return nil, true, nil
	}
	return gc, true, nil
}

func (r *classResolver) managedClass(ctx context.Context, name string) (*gwv1.GatewayClass, error) {
	gc, _, err := r.lookupClass(ctx, name)
	return gc, err
}

// credentials resolves a class's credentials. An *invalidParamsError means
// the class is invalid; other errors are transient.
func (r *classResolver) credentials(ctx context.Context, gc *gwv1.GatewayClass) (*Credentials, error) {
	ref := gc.Spec.ParametersRef
	if ref == nil {
		return nil, &invalidParamsError{"spec.parametersRef must name a Secret with Cloudflare credentials"}
	}
	if string(ref.Group) != "" || string(ref.Kind) != "Secret" {
		return nil, &invalidParamsError{fmt.Sprintf("parametersRef must be a core Secret, not %s/%s", ref.Group, ref.Kind)}
	}
	if ref.Namespace == nil || *ref.Namespace == "" {
		return nil, &invalidParamsError{"parametersRef must set a namespace"}
	}
	sec := &corev1.Secret{}
	if err := r.reader().Get(ctx, types.NamespacedName{Namespace: string(*ref.Namespace), Name: ref.Name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &invalidParamsError{fmt.Sprintf("Secret %s/%s not found", *ref.Namespace, ref.Name)}
		}
		return nil, err
	}
	get := func(k string) string { return strings.TrimSpace(string(sec.Data[k])) }
	c := &Credentials{AccountID: get(KeyAccountID), AccountToken: get(KeyAPIToken), DNSToken: get(KeyDNSAPIToken), SecretVersion: string(sec.UID) + "/" + sec.ResourceVersion}
	switch {
	case c.AccountID != "" && c.AccountToken != "":
	case c.AccountID != "" || c.AccountToken != "":
		return nil, &invalidParamsError{fmt.Sprintf("Secret %s/%s must hold both %q and %q, or neither", *ref.Namespace, ref.Name, KeyAccountID, KeyAPIToken)}
	case r.Config.HasAccountCredentials():
		c.AccountID, c.AccountToken = r.Config.AccountID, r.Config.APIToken
	default:
		return nil, &invalidParamsError{fmt.Sprintf("Secret %s/%s holds no %q and %q, and the controller has no account credentials", *ref.Namespace, ref.Name, KeyAccountID, KeyAPIToken)}
	}
	if c.DNSToken == "" {
		c.DNSToken = c.AccountToken
	}
	return c, nil
}

// fallbackCredentials are used to clean up a Gateway whose class is gone:
// the controller-level account credentials, if any.
func (r *classResolver) fallbackCredentials() *Credentials {
	if !r.Config.HasAccountCredentials() {
		return nil
	}
	return &Credentials{AccountID: r.Config.AccountID, AccountToken: r.Config.APIToken, DNSToken: r.Config.APIToken}
}

// gatewayCredentials resolves the credentials of a Gateway's class.
func (r *classResolver) gatewayCredentials(ctx context.Context, gw *gwv1.Gateway) (*Credentials, error) {
	gc, err := r.managedClass(ctx, string(gw.Spec.GatewayClassName))
	if err != nil {
		return nil, err
	}
	if gc == nil {
		return nil, &invalidParamsError{fmt.Sprintf("GatewayClass %q is not served by this controller", gw.Spec.GatewayClassName)}
	}
	return r.credentials(ctx, gc)
}

// ---- zone lookup ----

// zoneCache remembers zone lookups for a while: zones change rarely, and
// every hostname needs one.
type zoneCache struct {
	mu      sync.Mutex
	entries map[string]zoneEntry // token-hash|name
}

type zoneEntry struct {
	zone *cloudflare.Zone
	at   time.Time
}

const zoneCacheTTL = 5 * time.Minute

var zones = &zoneCache{entries: map[string]zoneEntry{}}

// findZone returns the zone of a hostname: the hostname or its longest
// parent suffix that is an active zone visible to the token.
func findZone(ctx context.Context, cf *cloudflare.Client, token, accountID, host string) (*cloudflare.Zone, error) {
	name := strings.TrimPrefix(strings.ToLower(strings.TrimSuffix(host, ".")), "*.")
	labels := strings.Split(name, ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		key := cf.BaseURL + "|" + tokenKey(token) + "|" + candidate
		zones.mu.Lock()
		e, ok := zones.entries[key]
		zones.mu.Unlock()
		if !ok || time.Since(e.at) > zoneCacheTTL {
			z, err := cf.FindZone(ctx, token, "", candidate)
			if err != nil {
				return nil, err
			}
			e = zoneEntry{zone: z, at: time.Now()}
			zones.mu.Lock()
			zones.entries[key] = e
			zones.mu.Unlock()
		}
		if e.zone != nil {
			return e.zone, nil
		}
	}
	return nil, nil
}

// tokenKey identifies a token in caches without keeping the token itself.
func tokenKey(token string) string {
	return hashString(token)
}
