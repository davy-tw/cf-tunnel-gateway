// Package cftest is an in-memory fake of the Cloudflare API endpoints the
// controller uses, for tests and for the end-to-end environment.
package cftest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
)

// Token is what a fake API token may do.
type Token struct {
	// Status is the verification status; "" means active.
	Status string
	// Account may manage tunnels and Access applications in Account.
	Account string
	// Zones the token may read and edit DNS in; "*" means all.
	Zones []string
	// AccountScoped tokens verify only through the account endpoint.
	AccountScoped bool
}

// Server is the fake API. The zero value is not usable; call New.
type Server struct {
	mu       sync.Mutex
	tokens   map[string]*Token
	zones    map[string]*cloudflare.Zone // by ID
	records  map[string]map[string]*cloudflare.DNSRecord
	tunnels  map[string]*tunnel
	apps     map[string]*cloudflare.AccessApp
	limit    int // rate-limit the next n calls
	calls    []string
	failNext map[string]int // operation key -> count
}

type tunnel struct {
	cloudflare.Tunnel
	account string
	secret  string
	config  json.RawMessage
}

// New returns an empty fake.
func New() *Server {
	return &Server{
		tokens:   map[string]*Token{},
		zones:    map[string]*cloudflare.Zone{},
		records:  map[string]map[string]*cloudflare.DNSRecord{},
		tunnels:  map[string]*tunnel{},
		apps:     map[string]*cloudflare.AccessApp{},
		failNext: map[string]int{},
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	h := newID()
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// AddToken registers a token.
func (s *Server) AddToken(value string, t Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[value] = &t
}

// AddZone registers an active zone and returns its ID.
func (s *Server) AddZone(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID()
	s.zones[id] = &cloudflare.Zone{ID: id, Name: name, Status: "active"}
	s.records[id] = map[string]*cloudflare.DNSRecord{}
	return id
}

// AddRecord adds a DNS record directly (a record made by someone else).
func (s *Server) AddRecord(zoneID string, r cloudflare.DNSRecord) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = newID()
	s.records[zoneID][r.ID] = &r
	return r.ID
}

// AddTunnel adds a remotely-managed tunnel directly and returns its ID.
func (s *Server) AddTunnel(account, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newUUID()
	s.tunnels[id] = &tunnel{Tunnel: cloudflare.Tunnel{ID: id, Name: name, Status: "inactive", ConfigSrc: "cloudflare"}, account: account, secret: "x"}
	return id
}

// SetConnections gives a tunnel n active connector connections.
func (s *Server) SetConnections(id string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tunnels[id]
	if t == nil {
		return
	}
	t.Connections = nil
	for i := 0; i < n; i++ {
		t.Connections = append(t.Connections, cloudflare.TunnelConnection{ID: newUUID(), ColoName: "fake"})
	}
	t.Status = "inactive"
	if n > 0 {
		t.Status = "healthy"
	}
}

// ConnectAll gives every tunnel without connections one connection.
func (s *Server) ConnectAll() {
	s.mu.Lock()
	ids := []string{}
	for id, t := range s.tunnels {
		if len(t.Connections) == 0 && t.DeletedAt == nil {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.SetConnections(id, 1)
	}
}

// RateLimit makes the next n calls answer HTTP 429.
func (s *Server) RateLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limit = n
}

// FailNext makes the next n calls of "METHOD path-prefix" fail with 500.
func (s *Server) FailNext(key string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[key] = n
}

// Tunnels returns the tunnels that are not deleted.
func (s *Server) Tunnels() []cloudflare.Tunnel {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cloudflare.Tunnel
	for _, t := range s.tunnels {
		if t.DeletedAt == nil {
			out = append(out, t.Tunnel)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tunnel returns a tunnel by ID, deleted or not.
func (s *Server) Tunnel(id string) (cloudflare.Tunnel, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tunnels[id]
	if !ok {
		return cloudflare.Tunnel{}, "", false
	}
	return t.Tunnel, t.secret, true
}

// Config returns a tunnel's remote configuration.
func (s *Server) Config(id string) *cloudflare.TunnelConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tunnels[id]
	if t == nil || t.config == nil {
		return nil
	}
	var c cloudflare.TunnelConfig
	_ = json.Unmarshal(t.config, &c)
	return &c
}

// Records returns every record of a zone, sorted by type and name.
func (s *Server) Records(zoneID string) []cloudflare.DNSRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cloudflare.DNSRecord
	for _, r := range s.records[zoneID] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AccessApps returns the Access applications.
func (s *Server) AccessApps() []cloudflare.AccessApp {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cloudflare.AccessApp
	for _, a := range s.apps {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// Calls returns the "METHOD path" of every call so far.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// ---- HTTP ----

type info struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func write(w http.ResponseWriter, status int, result any, errs ...info) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"success": status < 300, "errors": errs, "messages": []info{}, "result": result}
	if errs == nil {
		body["errors"] = []info{}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeList[T any](w http.ResponseWriter, r *http.Request, items []T) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 20
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	total := (len(items) + per - 1) / per
	if total == 0 {
		total = 1
	}
	lo, hi := (page-1)*per, page*per
	if lo > len(items) {
		lo = len(items)
	}
	if hi > len(items) {
		hi = len(items)
	}
	out := items[lo:hi]
	if out == nil {
		out = []T{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []info{}, "messages": []info{}, "result": out,
		"result_info": map[string]int{"page": page, "per_page": per, "total_pages": total, "count": len(out), "total_count": len(items)},
	})
}

func notFound(w http.ResponseWriter) { write(w, 404, nil, info{Code: 1000, Message: "not found"}) }
func forbidden(w http.ResponseWriter) {
	write(w, 403, nil, info{Code: 10000, Message: "Authentication error"})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	s.calls = append(s.calls, r.Method+" "+path)
	if s.limit > 0 {
		s.limit--
		w.Header().Set("Retry-After", "1")
		write(w, 429, nil, info{Code: 10429, Message: "rate limited"})
		return
	}
	for key, n := range s.failNext {
		m, p, _ := strings.Cut(key, " ")
		if n > 0 && r.Method == m && strings.HasPrefix(path, p) {
			s.failNext[key] = n - 1
			write(w, 500, nil, info{Code: 1, Message: "injected failure"})
			return
		}
	}
	tok := s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if tok == nil {
		write(w, 401, nil, info{Code: 1000, Message: "Invalid API Token"})
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/user/tokens/verify":
		if tok.AccountScoped {
			write(w, 401, nil, info{Code: 1000, Message: "Invalid API Token"})
			return
		}
		s.verify(w, tok)
	case len(parts) == 4 && parts[0] == "accounts" && parts[2] == "tokens" && parts[3] == "verify":
		if !tok.AccountScoped || tok.Account != parts[1] {
			write(w, 401, nil, info{Code: 1000, Message: "Invalid API Token"})
			return
		}
		s.verify(w, tok)
	case len(parts) >= 3 && parts[0] == "accounts" && parts[2] == "cfd_tunnel":
		if tok.Account != parts[1] {
			forbidden(w)
			return
		}
		s.tunnelAPI(w, r, parts[1], parts[3:])
	case len(parts) >= 4 && parts[0] == "accounts" && parts[2] == "access" && parts[3] == "apps":
		if tok.Account != parts[1] {
			forbidden(w)
			return
		}
		s.accessAPI(w, r, parts[4:])
	case path == "/zones":
		s.listZones(w, r, tok)
	case len(parts) >= 3 && parts[0] == "zones" && parts[2] == "dns_records":
		z := s.zones[parts[1]]
		if z == nil {
			notFound(w)
			return
		}
		if !tokZone(tok, z.Name) {
			forbidden(w)
			return
		}
		s.dnsAPI(w, r, z.ID, parts[3:])
	default:
		notFound(w)
	}
}

func tokZone(t *Token, name string) bool {
	for _, z := range t.Zones {
		if z == "*" || strings.EqualFold(z, name) {
			return true
		}
	}
	return false
}

func (s *Server) verify(w http.ResponseWriter, t *Token) {
	st := t.Status
	if st == "" {
		st = "active"
	}
	write(w, 200, map[string]string{"id": "tok", "status": st})
}

func (s *Server) tunnelAPI(w http.ResponseWriter, r *http.Request, account string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query()
			var out []cloudflare.Tunnel
			for _, t := range s.tunnels {
				if t.account != account || (q.Get("is_deleted") == "false" && t.DeletedAt != nil) {
					continue
				}
				if n := q.Get("name"); n != "" && t.Name != n {
					continue
				}
				out = append(out, t.Tunnel)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			writeList(w, r, out)
		case http.MethodPost:
			var body struct {
				Name, ConfigSrc, TunnelSecret string
			}
			var raw map[string]string
			_ = json.NewDecoder(r.Body).Decode(&raw)
			body.Name, body.ConfigSrc, body.TunnelSecret = raw["name"], raw["config_src"], raw["tunnel_secret"]
			if body.Name == "" || body.TunnelSecret == "" {
				write(w, 400, nil, info{Code: 1003, Message: "name and tunnel_secret are required"})
				return
			}
			for _, t := range s.tunnels {
				if t.account == account && t.Name == body.Name && t.DeletedAt == nil {
					write(w, 409, nil, info{Code: 1013, Message: "You already have a tunnel with this name"})
					return
				}
			}
			id := newUUID()
			t := &tunnel{Tunnel: cloudflare.Tunnel{ID: id, Name: body.Name, Status: "inactive", ConfigSrc: body.ConfigSrc}, account: account, secret: body.TunnelSecret}
			s.tunnels[id] = t
			write(w, 200, t.Tunnel)
		default:
			notFound(w)
		}
		return
	}
	t := s.tunnels[rest[0]]
	if t == nil || t.account != account {
		notFound(w)
		return
	}
	sub := strings.Join(rest[1:], "/")
	switch {
	case sub == "" && r.Method == http.MethodGet:
		write(w, 200, t.Tunnel)
	case sub == "" && r.Method == http.MethodPatch:
		var raw map[string]string
		_ = json.NewDecoder(r.Body).Decode(&raw)
		if v := raw["tunnel_secret"]; v != "" {
			t.secret = v
		}
		if v := raw["name"]; v != "" {
			t.Name = v
		}
		write(w, 200, t.Tunnel)
	case sub == "" && r.Method == http.MethodDelete:
		if t.DeletedAt != nil {
			notFound(w)
			return
		}
		if len(t.Connections) > 0 {
			write(w, 400, nil, info{Code: 1022, Message: "Cannot delete a tunnel with active connections"})
			return
		}
		now := time.Now().UTC()
		t.DeletedAt = &now
		write(w, 200, t.Tunnel)
	case sub == "connections" && r.Method == http.MethodDelete:
		t.Connections = nil
		t.Status = "inactive"
		write(w, 200, nil)
	case sub == "token" && r.Method == http.MethodGet:
		write(w, 200, "fake-token."+t.ID+"."+t.secret)
	case sub == "configurations" && r.Method == http.MethodPut:
		var body struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Config) == 0 {
			write(w, 400, nil, info{Code: 1003, Message: "invalid configuration"})
			return
		}
		if msg := validateConfig(body.Config); msg != "" {
			write(w, 400, nil, info{Code: 1056, Message: msg})
			return
		}
		t.config = body.Config
		write(w, 200, map[string]any{"tunnel_id": t.ID, "config": body.Config})
	case sub == "configurations" && r.Method == http.MethodGet:
		write(w, 200, map[string]any{"tunnel_id": t.ID, "config": t.config})
	default:
		notFound(w)
	}
}

// validateConfig applies the checks cloudflared makes: a final catch-all,
// no catch-all before it, and wildcards only as the first label.
func validateConfig(raw json.RawMessage) string {
	var c cloudflare.TunnelConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return err.Error()
	}
	if len(c.Ingress) == 0 {
		return "no ingress rules"
	}
	for i, r := range c.Ingress {
		last := i == len(c.Ingress)-1
		catchAll := (r.Hostname == "" || r.Hostname == "*") && r.Path == ""
		if last && !catchAll {
			return "the last ingress rule must match all URLs"
		}
		if !last && catchAll {
			return "a rule before the last matches every URL"
		}
		if strings.Count(r.Hostname, "*") > 1 || (strings.Contains(r.Hostname, "*") && !strings.HasPrefix(r.Hostname, "*.") && r.Hostname != "*") {
			return "hostname patterns can only have a leading wildcard"
		}
		if r.Service == "" {
			return "a rule has no service"
		}
	}
	return ""
}

func (s *Server) listZones(w http.ResponseWriter, r *http.Request, tok *Token) {
	q := r.URL.Query()
	var out []cloudflare.Zone
	for _, z := range s.zones {
		if !tokZone(tok, z.Name) {
			continue
		}
		if n := q.Get("name"); n != "" && !strings.EqualFold(n, z.Name) {
			continue
		}
		out = append(out, *z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeList(w, r, out)
}

func (s *Server) dnsAPI(w http.ResponseWriter, r *http.Request, zone string, rest []string) {
	recs := s.records[zone]
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query()
			var out []cloudflare.DNSRecord
			for _, rec := range recs {
				if n := q.Get("name"); n != "" && !strings.EqualFold(n, rec.Name) {
					continue
				}
				if t := q.Get("type"); t != "" && t != rec.Type {
					continue
				}
				if c := q.Get("content.contains"); c != "" && !strings.Contains(rec.Content, c) {
					continue
				}
				out = append(out, *rec)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			writeList(w, r, out)
		case http.MethodPost:
			var rec cloudflare.DNSRecord
			if err := json.NewDecoder(r.Body).Decode(&rec); err != nil || rec.Name == "" || rec.Type == "" {
				write(w, 400, nil, info{Code: 9000, Message: "invalid record"})
				return
			}
			if len(rec.Comment) > 100 {
				write(w, 400, nil, info{Code: 9100, Message: "comment too long"})
				return
			}
			for _, o := range recs {
				if strings.EqualFold(o.Name, rec.Name) && (o.Type == "CNAME" || rec.Type == "CNAME") {
					write(w, 400, nil, info{Code: 81053, Message: "An A, AAAA, or CNAME record with that host already exists."})
					return
				}
			}
			rec.ID = newID()
			rec.Name = strings.ToLower(rec.Name)
			recs[rec.ID] = &rec
			write(w, 200, rec)
		default:
			notFound(w)
		}
		return
	}
	rec := recs[rest[0]]
	if rec == nil {
		notFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		write(w, 200, rec)
	case http.MethodPut, http.MethodPatch:
		var upd cloudflare.DNSRecord
		_ = json.NewDecoder(r.Body).Decode(&upd)
		upd.ID = rec.ID
		if upd.Name == "" {
			upd.Name = rec.Name
		}
		*rec = upd
		write(w, 200, rec)
	case http.MethodDelete:
		delete(recs, rec.ID)
		write(w, 200, map[string]string{"id": rec.ID})
	default:
		notFound(w)
	}
}

func (s *Server) accessAPI(w http.ResponseWriter, r *http.Request, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			var out []cloudflare.AccessApp
			for _, a := range s.apps {
				out = append(out, *a)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			writeList(w, r, out)
		case http.MethodPost:
			var a cloudflare.AccessApp
			_ = json.NewDecoder(r.Body).Decode(&a)
			a.ID = newUUID()
			s.apps[a.ID] = &a
			write(w, 200, a)
		default:
			notFound(w)
		}
		return
	}
	a := s.apps[rest[0]]
	if a == nil {
		notFound(w)
		return
	}
	if r.Method == http.MethodDelete {
		delete(s.apps, a.ID)
		write(w, 200, map[string]string{"id": a.ID})
		return
	}
	write(w, 200, a)
}
