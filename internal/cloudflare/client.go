// Package cloudflare is a small client for the parts of the Cloudflare API
// the controller uses: token verification, Cloudflare Tunnel, zones, DNS
// records and Access applications.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls the Cloudflare API. Credentials are passed per call, since
// every GatewayClass may use its own tokens.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Now is used for Retry-After dates (tests).
	Now func() time.Time
}

// New returns a client for baseURL (for example
// "https://api.cloudflare.com/client/v4").
func New(baseURL string) *Client {
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}
}

// DefaultRateLimitBackoff is how long to wait after HTTP 429 without a
// Retry-After header.
const DefaultRateLimitBackoff = 10 * time.Minute

// APIError is an unsuccessful Cloudflare API response.
type APIError struct {
	Op     string
	Status int
	Errors []ResponseInfo
}

// ResponseInfo is one entry of a response's errors or messages.
type ResponseInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	var msgs []string
	for _, x := range e.Errors {
		msgs = append(msgs, fmt.Sprintf("%s (code %d)", x.Message, x.Code))
	}
	if len(msgs) == 0 {
		msgs = append(msgs, http.StatusText(e.Status))
	}
	return fmt.Sprintf("cloudflare %s: HTTP %d: %s", e.Op, e.Status, strings.Join(msgs, "; "))
}

// RateLimitError is HTTP 429.
type RateLimitError struct {
	Op         string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("cloudflare %s: rate limited, retrying in %s", e.Op, e.RetryAfter)
}

// IsNotFound reports whether err is an HTTP 404 from the API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsAuth reports whether err is an authentication or authorization failure.
func IsAuth(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// RetryAfter returns how long to wait when err is a rate limit.
func RetryAfter(err error) (time.Duration, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []ResponseInfo  `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// do sends a request and decodes the result into out. It returns the total
// number of pages for list calls.
func (c *Client) do(ctx context.Context, op, token, method, path string, query url.Values, body, out any) (int, error) {
	start := time.Now()
	pages, err := c.send(ctx, op, token, method, path, query, body, out)
	observe(op, start, err)
	return pages, err
}

func (c *Client) send(ctx context.Context, op, token, method, path string, query url.Values, body, out any) (int, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL holds no secrets, but keep the message short.
		return 0, fmt.Errorf("cloudflare %s: %w", op, unwrapURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, fmt.Errorf("cloudflare %s: reading response: %w", op, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, &RateLimitError{Op: op, RetryAfter: c.retryAfter(resp.Header.Get("Retry-After"))}
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		if resp.StatusCode >= 300 {
			return 0, &APIError{Op: op, Status: resp.StatusCode}
		}
		return 0, fmt.Errorf("cloudflare %s: decoding response: %w", op, err)
	}
	if resp.StatusCode >= 300 || !env.Success {
		status := resp.StatusCode
		if status < 300 {
			status = http.StatusBadRequest
		}
		return 0, &APIError{Op: op, Status: status, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return 0, fmt.Errorf("cloudflare %s: decoding result: %w", op, err)
		}
	}
	pages := 1
	if env.ResultInfo != nil && env.ResultInfo.TotalPages > 0 {
		pages = env.ResultInfo.TotalPages
	}
	return pages, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *Client) retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultRateLimitBackoff
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(c.Now()); d > 0 {
			return d
		}
		return time.Second
	}
	return DefaultRateLimitBackoff
}

// listAll fetches every page of a list call.
func listAll[T any](ctx context.Context, c *Client, op, token, path string, query url.Values) ([]T, error) {
	var all []T
	if query == nil {
		query = url.Values{}
	}
	query.Set("per_page", "100")
	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))
		var items []T
		pages, err := c.do(ctx, op, token, http.MethodGet, path, query, nil, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if page >= pages || len(items) == 0 || page >= 1000 {
			return all, nil
		}
	}
}

// ---- tokens ----

// TokenStatus is the result of token verification.
type TokenStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// VerifyToken checks a token with the user-token endpoint first and the
// account-token endpoint second (when accountID is known). It returns an
// error unless the token is active.
func (c *Client) VerifyToken(ctx context.Context, token, accountID string) error {
	var st TokenStatus
	_, err := c.do(ctx, "verify user token", token, http.MethodGet, "/user/tokens/verify", nil, nil, &st)
	if err != nil && accountID != "" {
		var st2 TokenStatus
		if _, err2 := c.do(ctx, "verify account token", token, http.MethodGet, "/accounts/"+url.PathEscape(accountID)+"/tokens/verify", nil, nil, &st2); err2 == nil {
			st, err = st2, nil
		} else if _, rl := RetryAfter(err2); rl {
			return err2
		}
	}
	if err != nil {
		return err
	}
	if st.Status != "active" {
		return fmt.Errorf("token status is %q, not active", st.Status)
	}
	return nil
}

// ---- tunnels ----

// Tunnel is a Cloudflare Tunnel.
type Tunnel struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	ConfigSrc   string             `json:"config_src,omitempty"`
	DeletedAt   *time.Time         `json:"deleted_at,omitempty"`
	Connections []TunnelConnection `json:"connections"`
}

// TunnelConnection is one connection of a connector to Cloudflare's edge.
type TunnelConnection struct {
	ID                 string `json:"id"`
	ColoName           string `json:"colo_name"`
	IsPendingReconnect bool   `json:"is_pending_reconnect"`
}

// Deleted reports whether the tunnel has been deleted.
func (t *Tunnel) Deleted() bool { return t.DeletedAt != nil && !t.DeletedAt.IsZero() }

// ActiveConnections counts connections that are not pending a reconnect.
func (t *Tunnel) ActiveConnections() int {
	n := 0
	for _, c := range t.Connections {
		if !c.IsPendingReconnect {
			n++
		}
	}
	return n
}

// CNAMETarget is the DNS target that routes a hostname to the tunnel.
func CNAMETarget(tunnelID string) string { return tunnelID + ".cfargotunnel.com" }

func tunnelPath(accountID string, parts ...string) string {
	p := "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel"
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// ListTunnels returns the non-deleted tunnels with the given name (all of
// them when name is empty).
func (c *Client) ListTunnels(ctx context.Context, accountID, token, name string) ([]Tunnel, error) {
	q := url.Values{"is_deleted": {"false"}}
	if name != "" {
		q.Set("name", name)
	}
	return listAll[Tunnel](ctx, c, "list tunnels", token, tunnelPath(accountID), q)
}

// CanListTunnels checks that the token can read the account's tunnels.
func (c *Client) CanListTunnels(ctx context.Context, accountID, token string) error {
	var out []Tunnel
	_, err := c.do(ctx, "list tunnels", token, http.MethodGet, tunnelPath(accountID), url.Values{"per_page": {"1"}, "is_deleted": {"false"}}, nil, &out)
	return err
}

// GetTunnel returns a tunnel by ID.
func (c *Client) GetTunnel(ctx context.Context, accountID, token, id string) (*Tunnel, error) {
	var t Tunnel
	if _, err := c.do(ctx, "get tunnel", token, http.MethodGet, tunnelPath(accountID, id), nil, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTunnel creates a remotely-managed tunnel. secret is the base64 of
// at least 32 random bytes.
func (c *Client) CreateTunnel(ctx context.Context, accountID, token, name, secret string) (*Tunnel, error) {
	var t Tunnel
	body := map[string]string{"name": name, "config_src": "cloudflare", "tunnel_secret": secret}
	if _, err := c.do(ctx, "create tunnel", token, http.MethodPost, tunnelPath(accountID), nil, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// RotateTunnelSecret replaces the tunnel secret, so that connectors using
// the old token cannot connect again.
func (c *Client) RotateTunnelSecret(ctx context.Context, accountID, token, id, secret string) error {
	_, err := c.do(ctx, "rotate tunnel secret", token, http.MethodPatch, tunnelPath(accountID, id), nil, map[string]string{"tunnel_secret": secret}, nil)
	return err
}

// DeleteTunnelConnections drops every connection of the tunnel.
func (c *Client) DeleteTunnelConnections(ctx context.Context, accountID, token, id string) error {
	_, err := c.do(ctx, "delete tunnel connections", token, http.MethodDelete, tunnelPath(accountID, id, "connections"), nil, nil, nil)
	return err
}

// DeleteTunnel deletes a tunnel.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, token, id string) error {
	_, err := c.do(ctx, "delete tunnel", token, http.MethodDelete, tunnelPath(accountID, id), nil, nil, nil)
	return err
}

// TunnelToken returns the connector token of a tunnel.
func (c *Client) TunnelToken(ctx context.Context, accountID, token, id string) (string, error) {
	var t string
	if _, err := c.do(ctx, "get tunnel token", token, http.MethodGet, tunnelPath(accountID, id, "token"), nil, nil, &t); err != nil {
		return "", err
	}
	if t == "" {
		return "", fmt.Errorf("cloudflare get tunnel token: empty token")
	}
	return t, nil
}

// PutTunnelConfig replaces the tunnel's remote configuration.
func (c *Client) PutTunnelConfig(ctx context.Context, accountID, token, id string, cfg *TunnelConfig) error {
	_, err := c.do(ctx, "put tunnel configuration", token, http.MethodPut, tunnelPath(accountID, id, "configurations"), nil,
		map[string]any{"config": cfg}, nil)
	return err
}

// GetTunnelConfig returns the tunnel's remote configuration.
func (c *Client) GetTunnelConfig(ctx context.Context, accountID, token, id string) (*TunnelConfig, error) {
	var out struct {
		Config *TunnelConfig `json:"config"`
	}
	if _, err := c.do(ctx, "get tunnel configuration", token, http.MethodGet, tunnelPath(accountID, id, "configurations"), nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Config, nil
}

// ---- zones ----

// Zone is a Cloudflare zone.
type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// FindZone returns the active zone with exactly this name, or nil.
func (c *Client) FindZone(ctx context.Context, token, accountID, name string) (*Zone, error) {
	q := url.Values{"name": {name}, "status": {"active"}}
	if accountID != "" {
		q.Set("account.id", accountID)
	}
	zones, err := listAll[Zone](ctx, c, "list zones", token, "/zones", q)
	if err != nil {
		return nil, err
	}
	for i := range zones {
		if strings.EqualFold(zones[i].Name, name) {
			return &zones[i], nil
		}
	}
	return nil, nil
}

// CountZones returns how many zones the token can see (at most one page).
func (c *Client) CountZones(ctx context.Context, token, accountID string) (int, error) {
	q := url.Values{"per_page": {"5"}}
	if accountID != "" {
		q.Set("account.id", accountID)
	}
	var zones []Zone
	if _, err := c.do(ctx, "list zones", token, http.MethodGet, "/zones", q, nil, &zones); err != nil {
		return 0, err
	}
	return len(zones), nil
}

// ---- DNS ----

// DNSRecord is a DNS record.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied *bool  `json:"proxied,omitempty"`
	TTL     int    `json:"ttl,omitempty"`
	Comment string `json:"comment,omitempty"`
}

func dnsPath(zoneID string, parts ...string) string {
	p := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// ListDNSRecords returns the records with this name (and type, when set).
func (c *Client) ListDNSRecords(ctx context.Context, token, zoneID, name, typ string) ([]DNSRecord, error) {
	q := url.Values{"name": {name}}
	if typ != "" {
		q.Set("type", typ)
	}
	return listAll[DNSRecord](ctx, c, "list DNS records", token, dnsPath(zoneID), q)
}

// ListDNSRecordsByContent returns the TXT records whose content contains s.
func (c *Client) ListDNSRecordsByContent(ctx context.Context, token, zoneID, typ, s string) ([]DNSRecord, error) {
	q := url.Values{"type": {typ}, "content.contains": {s}}
	return listAll[DNSRecord](ctx, c, "list DNS records", token, dnsPath(zoneID), q)
}

// CreateDNSRecord creates a record.
func (c *Client) CreateDNSRecord(ctx context.Context, token, zoneID string, r DNSRecord) (*DNSRecord, error) {
	var out DNSRecord
	if _, err := c.do(ctx, "create DNS record", token, http.MethodPost, dnsPath(zoneID), nil, r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDNSRecord overwrites a record.
func (c *Client) UpdateDNSRecord(ctx context.Context, token, zoneID, id string, r DNSRecord) error {
	r.ID = ""
	_, err := c.do(ctx, "update DNS record", token, http.MethodPut, dnsPath(zoneID, id), nil, r, nil)
	return err
}

// DeleteDNSRecord deletes a record; a missing record is not an error.
func (c *Client) DeleteDNSRecord(ctx context.Context, token, zoneID, id string) error {
	_, err := c.do(ctx, "delete DNS record", token, http.MethodDelete, dnsPath(zoneID, id), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ---- Access ----

// AccessApp is a Cloudflare Access application.
type AccessApp struct {
	ID              string `json:"id,omitempty"`
	Name            string `json:"name"`
	Domain          string `json:"domain"`
	Type            string `json:"type"`
	SessionDuration string `json:"session_duration,omitempty"`
}

func accessPath(accountID string, parts ...string) string {
	p := "/accounts/" + url.PathEscape(accountID) + "/access/apps"
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// ListAccessApps returns the account's Access applications.
func (c *Client) ListAccessApps(ctx context.Context, accountID, token string) ([]AccessApp, error) {
	return listAll[AccessApp](ctx, c, "list Access applications", token, accessPath(accountID), nil)
}

// CreateAccessApp creates a self-hosted Access application.
func (c *Client) CreateAccessApp(ctx context.Context, accountID, token string, app AccessApp) (*AccessApp, error) {
	var out AccessApp
	if _, err := c.do(ctx, "create Access application", token, http.MethodPost, accessPath(accountID), nil, app, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAccessApp deletes an Access application; a missing one is not an
// error.
func (c *Client) DeleteAccessApp(ctx context.Context, accountID, token, id string) error {
	_, err := c.do(ctx, "delete Access application", token, http.MethodDelete, accessPath(accountID, id), nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
