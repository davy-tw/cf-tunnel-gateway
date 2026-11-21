// Package config holds the controller-wide settings and the names this
// project owns.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Domain prefixes every annotation, finalizer and controller name owned by
// this project.
const Domain = "cf-tunnel-gateway.davy.tw"

// DefaultInstance is the default instance name, and the default name of the
// classes and the shared Gateway.
const DefaultInstance = "cf-tunnel-gateway"

// Well-known names derived from Domain.
const (
	DefaultControllerName = Domain + "/gateway-controller"
	IngressControllerName = Domain + "/ingress-controller"
	Finalizer             = Domain + "/finalizer"

	// User-set Gateway annotations.
	AnnotationTunnelID    = Domain + "/tunnel-id"
	AnnotationKeepTunnel  = Domain + "/keep-tunnel"
	AnnotationDNS         = Domain + "/dns"
	AnnotationSkipCleanup = Domain + "/skip-cleanup"

	// Controller-written Gateway annotations.
	AnnotationTunnel         = Domain + "/tunnel"
	AnnotationPreviousTunnel = Domain + "/previous-tunnel"
	AnnotationTokenSecret    = Domain + "/token-secret"
	AnnotationHostnames      = Domain + "/hostnames"
	AnnotationGRPCNotified   = Domain + "/grpc-notified"
	AnnotationDNSHostnames   = Domain + "/dns-hostnames"

	// Ingress and Service annotations.
	AnnotationGateway         = Domain + "/gateway"
	AnnotationParameters      = Domain + "/parameters"
	AnnotationHostname        = Domain + "/hostname"
	AnnotationTCPHostnames    = Domain + "/tcp-hostnames"
	AnnotationDefaultHostname = Domain + "/default-hostname"

	// Namespace policy annotations.
	AnnotationAllowedGatewayClasses = Domain + "/allowed-gateway-classes"
	AnnotationGatewayClass          = Domain + "/gateway-class"
	AnnotationAllowedHostnames      = Domain + "/allowed-hostnames"
	AnnotationBaseDomain            = Domain + "/base-domain"

	// Annotations on generated objects.
	AnnotationSource    = Domain + "/source"
	AnnotationManaged   = Domain + "/managed"
	AnnotationDNSParent = Domain + "/dns-parent"
	// AnnotationSwitch on a generated route records the progress of a
	// switch between parents: "<phase>@<RFC 3339 time>".
	AnnotationSwitch   = Domain + "/switch"
	AnnotationInstance = Domain + "/instance"

	LabelSourceKind = Domain + "/source-kind"
	LabelSourceName = Domain + "/source-name"

	SourceIngress = "ingress"
	SourceService = "service"
	ManagedShared = "shared"

	GatewayShared    = "shared"
	GatewayDedicated = "dedicated"
)

// DefaultConnectorImage is the pinned cloudflared image.
const DefaultConnectorImage = "cloudflare/cloudflared:2026.11.1"

// Shared Gateway scopes.
const (
	ScopeNamespace = "namespace"
	ScopeCluster   = "cluster"
)

// DNS policies.
const (
	DNSPolicySync       = "sync"
	DNSPolicyUpsertOnly = "upsert-only"
)

// Hostname templates.
const (
	TemplateNameNamespace = "name-namespace"
	TemplateName          = "name"
)

// Config is the controller-wide configuration.
type Config struct {
	AccountID string
	APIToken  string
	APIURL    string

	Instance          string
	ControllerName    string
	GatewayClass      string
	IngressClass      string
	LoadBalancerClass string
	IngressEnabled    bool
	ServiceEnabled    bool

	SharedScope      string
	SharedName       string
	SharedNamespace  string
	SharedParameters string

	BaseDomain       string
	HostnameTemplate string
	TCPMode          bool
	AccessApps       bool

	DNSPolicy    string
	DNSOwnerID   string
	DNSTXTPrefix string
	DNSComment   string
	// DenyUnlistedHostnames makes namespaces without an allowed-hostnames
	// annotation publish nothing.
	DenyUnlistedHostnames bool

	WatchNamespaces []string
	SwitchTimeout   time.Duration
	SwitchGrace     time.Duration

	ConnectorImage string
	ClusterDomain  string
	MetricsAddr    string
	ProbeAddr      string
	// Namespace the controller runs in.
	Namespace string
}

// HasAccountCredentials reports whether controller-level account
// credentials are configured.
func (c *Config) HasAccountCredentials() bool { return c.AccountID != "" && c.APIToken != "" }

// Environment variable names.
const (
	EnvAccountID         = "CLOUDFLARE_ACCOUNT_ID"
	EnvAPIToken          = "CLOUDFLARE_API_TOKEN"
	EnvAPIURL            = "CLOUDFLARE_API_URL"
	EnvInstance          = "INSTANCE_NAME"
	EnvControllerName    = "GATEWAY_CONTROLLER_NAME"
	EnvGatewayClass      = "GATEWAY_CLASS"
	EnvIngressClass      = "INGRESS_CLASS"
	EnvLoadBalancerClass = "LOAD_BALANCER_CLASS"
	EnvIngressEnabled    = "INGRESS_ENABLED"
	EnvServiceEnabled    = "LOAD_BALANCER_ENABLED"
	EnvSharedScope       = "SHARED_GATEWAY_SCOPE"
	EnvSharedName        = "SHARED_GATEWAY_NAME"
	EnvSharedNamespace   = "SHARED_GATEWAY_NAMESPACE"
	EnvSharedParameters  = "SHARED_GATEWAY_PARAMETERS"
	EnvBaseDomain        = "DEFAULT_BASE_DOMAIN"
	EnvHostnameTemplate  = "DEFAULT_HOSTNAME_TEMPLATE"
	EnvTCPMode           = "TCP_MODE"
	EnvAccessApps        = "ACCESS_APPLICATIONS"
	EnvDNSPolicy         = "DNS_POLICY"
	EnvDNSOwnerID        = "DNS_OWNER_ID"
	EnvDNSTXTPrefix      = "DNS_TXT_PREFIX"
	EnvDNSComment        = "DNS_COMMENT"
	EnvHostnameDefault   = "HOSTNAME_ALLOWLIST_DEFAULT"
	EnvWatchNamespaces   = "WATCH_NAMESPACES"
	EnvSwitchTimeout     = "SWITCH_TIMEOUT"
	EnvSwitchGrace       = "SWITCH_GRACE_PERIOD"
	EnvConnectorImage    = "CLOUDFLARED_IMAGE"
	EnvClusterDomain     = "CLUSTER_DOMAIN"
	EnvMetricsAddr       = "METRICS_BIND_ADDRESS"
	EnvProbeAddr         = "HEALTH_PROBE_BIND_ADDRESS"
	EnvNamespace         = "POD_NAMESPACE"
)

// FromEnv reads the configuration from the process environment.
func FromEnv() (*Config, error) { return Load(os.Getenv) }

// Load reads the configuration through getenv.
func Load(getenv func(string) string) (*Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	var errs []string
	boolean := func(k string, def bool) bool {
		v := get(k, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %q is not a boolean", k, v))
		}
		return b
	}
	duration := func(k string, def time.Duration) time.Duration {
		v := get(k, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("%s: %q is not a valid duration", k, v))
		}
		return d
	}
	instance := get(EnvInstance, DefaultInstance)
	controllerName := DefaultControllerName
	if instance != DefaultInstance {
		controllerName = DefaultControllerName + "-" + instance
	}
	ns := get(EnvNamespace, "")
	c := &Config{
		AccountID:         get(EnvAccountID, ""),
		APIToken:          get(EnvAPIToken, ""),
		APIURL:            strings.TrimSuffix(get(EnvAPIURL, "https://api.cloudflare.com/client/v4"), "/"),
		Instance:          instance,
		ControllerName:    get(EnvControllerName, controllerName),
		GatewayClass:      get(EnvGatewayClass, instance),
		IngressClass:      get(EnvIngressClass, instance),
		LoadBalancerClass: get(EnvLoadBalancerClass, instance),
		IngressEnabled:    boolean(EnvIngressEnabled, true),
		ServiceEnabled:    boolean(EnvServiceEnabled, true),
		SharedScope:       get(EnvSharedScope, ScopeNamespace),
		SharedName:        get(EnvSharedName, instance),
		SharedNamespace:   get(EnvSharedNamespace, ns),
		SharedParameters:  get(EnvSharedParameters, ""),
		BaseDomain:        strings.ToLower(strings.Trim(get(EnvBaseDomain, ""), ".")),
		HostnameTemplate:  get(EnvHostnameTemplate, TemplateNameNamespace),
		TCPMode:           boolean(EnvTCPMode, false),
		AccessApps:        boolean(EnvAccessApps, false),
		DNSPolicy:         get(EnvDNSPolicy, DNSPolicySync),
		DNSOwnerID:        get(EnvDNSOwnerID, instance),
		DNSTXTPrefix:      get(EnvDNSTXTPrefix, "_cf-tunnel-gateway"),
		DNSComment:        get(EnvDNSComment, "managed by cf-tunnel-gateway"),
		SwitchTimeout:     duration(EnvSwitchTimeout, 5*time.Minute),
		SwitchGrace:       duration(EnvSwitchGrace, 30*time.Second),
		ConnectorImage:    get(EnvConnectorImage, DefaultConnectorImage),
		ClusterDomain:     get(EnvClusterDomain, "cluster.local"),
		MetricsAddr:       get(EnvMetricsAddr, ":8080"),
		ProbeAddr:         get(EnvProbeAddr, ":8081"),
		Namespace:         ns,
	}
	switch v := get(EnvHostnameDefault, "allow"); v {
	case "allow":
	case "deny":
		c.DenyUnlistedHostnames = true
	default:
		errs = append(errs, fmt.Sprintf("%s: %q must be allow or deny", EnvHostnameDefault, v))
	}
	for _, n := range strings.Split(get(EnvWatchNamespaces, ""), ",") {
		if n = strings.TrimSpace(n); n != "" {
			c.WatchNamespaces = append(c.WatchNamespaces, n)
		}
	}
	if (c.AccountID == "") != (c.APIToken == "") {
		errs = append(errs, fmt.Sprintf("%s and %s must be set together", EnvAccountID, EnvAPIToken))
	}
	if e := validation.IsDNS1123Label(c.Instance); len(e) > 0 {
		errs = append(errs, fmt.Sprintf("%s %q: %s", EnvInstance, c.Instance, strings.Join(e, "; ")))
	}
	if e := validation.IsQualifiedName(c.LoadBalancerClass); len(e) > 0 {
		errs = append(errs, fmt.Sprintf("%s %q: %s", EnvLoadBalancerClass, c.LoadBalancerClass, strings.Join(e, "; ")))
	}
	if e := validation.IsDNS1123Subdomain(c.SharedName); len(e) > 0 {
		errs = append(errs, fmt.Sprintf("%s %q: %s", EnvSharedName, c.SharedName, strings.Join(e, "; ")))
	}
	if c.SharedScope != ScopeNamespace && c.SharedScope != ScopeCluster {
		errs = append(errs, fmt.Sprintf("%s: %q must be %s or %s", EnvSharedScope, c.SharedScope, ScopeNamespace, ScopeCluster))
	}
	if c.SharedScope == ScopeCluster && c.SharedNamespace == "" {
		errs = append(errs, fmt.Sprintf("%s (or %s) is needed with cluster scope", EnvSharedNamespace, EnvNamespace))
	}
	if c.HostnameTemplate != TemplateNameNamespace && c.HostnameTemplate != TemplateName {
		errs = append(errs, fmt.Sprintf("%s: %q must be %s or %s", EnvHostnameTemplate, c.HostnameTemplate, TemplateNameNamespace, TemplateName))
	}
	if c.DNSPolicy != DNSPolicySync && c.DNSPolicy != DNSPolicyUpsertOnly {
		errs = append(errs, fmt.Sprintf("%s: %q must be %s or %s", EnvDNSPolicy, c.DNSPolicy, DNSPolicySync, DNSPolicyUpsertOnly))
	}
	if e := validation.IsDNS1123Subdomain(strings.TrimPrefix(c.DNSTXTPrefix, "_")); len(e) > 0 || strings.Contains(c.DNSTXTPrefix, ".") {
		errs = append(errs, fmt.Sprintf("%s: %q must be a single DNS label (a leading underscore is allowed)", EnvDNSTXTPrefix, c.DNSTXTPrefix))
	}
	if c.BaseDomain != "" {
		if e := validation.IsDNS1123Subdomain(c.BaseDomain); len(e) > 0 || !strings.Contains(c.BaseDomain, ".") {
			errs = append(errs, fmt.Sprintf("%s: %q is not a domain", EnvBaseDomain, c.BaseDomain))
		}
	}
	if strings.ContainsAny(c.ConnectorImage, " \t\n") {
		errs = append(errs, fmt.Sprintf("%s: %q is not an image reference", EnvConnectorImage, c.ConnectorImage))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return c, nil
}
