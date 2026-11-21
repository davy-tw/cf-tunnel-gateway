package cloudflare

// TunnelConfig is a tunnel's remote configuration, as cloudflared reads it.
type TunnelConfig struct {
	Ingress       []IngressRule  `json:"ingress"`
	OriginRequest *OriginRequest `json:"originRequest,omitempty"`
}

// IngressRule maps a hostname and an optional path regular expression to
// an origin service. The last rule must have neither.
type IngressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Path          string         `json:"path,omitempty"`
	Service       string         `json:"service"`
	OriginRequest *OriginRequest `json:"originRequest,omitempty"`
}

// OriginRequest holds the origin request options the controller sets.
type OriginRequest struct {
	// KeepAliveConnections -1 disables cloudflared's idle connection pool
	// (0 does not; see docs/spike-findings.md).
	KeepAliveConnections *int   `json:"keepAliveConnections,omitempty"`
	HTTPHostHeader       string `json:"httpHostHeader,omitempty"`
	HTTP2Origin          *bool  `json:"http2Origin,omitempty"`
	NoTLSVerify          *bool  `json:"noTLSVerify,omitempty"`
}

// NotFoundService answers HTTP 404; it is the catch-all of every config.
const NotFoundService = "http_status:404"

// UnavailableService answers HTTP 503, for rules whose backend cannot be
// used.
const UnavailableService = "http_status:503"
