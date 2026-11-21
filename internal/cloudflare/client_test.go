package cloudflare_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare/cftest"
)

func setup(t *testing.T) (*cftest.Server, *cloudflare.Client) {
	t.Helper()
	fake := cftest.New()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return fake, cloudflare.New(srv.URL + "/client/v4")
}

func TestVerifyToken(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	fake.AddToken("user", cftest.Token{})
	fake.AddToken("acct", cftest.Token{AccountScoped: true, Account: "a1"})
	fake.AddToken("expired", cftest.Token{Status: "expired"})
	if err := c.VerifyToken(ctx, "user", ""); err != nil {
		t.Error(err)
	}
	if err := c.VerifyToken(ctx, "acct", "a1"); err != nil {
		t.Errorf("account token: %v", err)
	}
	if err := c.VerifyToken(ctx, "acct", ""); err == nil {
		t.Error("account token without an account ID verified")
	}
	if err := c.VerifyToken(ctx, "expired", ""); err == nil {
		t.Error("expired token verified")
	}
	if err := c.VerifyToken(ctx, "nope", "a1"); !cloudflare.IsAuth(err) {
		t.Errorf("unknown token: %v", err)
	}
}

func TestTunnelLifecycle(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	fake.AddToken("tok", cftest.Token{Account: "a1"})
	tun, err := c.CreateTunnel(ctx, "a1", "tok", "gw", "c2VjcmV0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTunnel(ctx, "a1", "tok", "gw", "c2VjcmV0"); err == nil {
		t.Error("duplicate name accepted")
	}
	list, err := c.ListTunnels(ctx, "a1", "tok", "gw")
	if err != nil || len(list) != 1 || list[0].ID != tun.ID {
		t.Fatalf("list: %v %v", list, err)
	}
	if err := c.CanListTunnels(ctx, "a1", "tok"); err != nil {
		t.Error(err)
	}
	if err := c.CanListTunnels(ctx, "other", "tok"); !cloudflare.IsAuth(err) {
		t.Errorf("other account: %v", err)
	}
	token, err := c.TunnelToken(ctx, "a1", "tok", tun.ID)
	if err != nil || token == "" {
		t.Fatalf("token: %q %v", token, err)
	}
	cfg := &cloudflare.TunnelConfig{
		OriginRequest: &cloudflare.OriginRequest{KeepAliveConnections: ptr.To(-1)},
		Ingress: []cloudflare.IngressRule{
			{Hostname: "a.example.com", Path: "^/api(?:/.*)?$", Service: "http://api.default.svc.cluster.local:80"},
			{Service: cloudflare.NotFoundService},
		},
	}
	if err := c.PutTunnelConfig(ctx, "a1", "tok", tun.ID, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetTunnelConfig(ctx, "a1", "tok", tun.ID)
	if err != nil || len(got.Ingress) != 2 || *got.OriginRequest.KeepAliveConnections != -1 {
		t.Fatalf("config: %+v %v", got, err)
	}
	bad := &cloudflare.TunnelConfig{Ingress: []cloudflare.IngressRule{{Hostname: "a.example.com", Service: "http://x"}}}
	if err := c.PutTunnelConfig(ctx, "a1", "tok", tun.ID, bad); err == nil {
		t.Error("config without a catch-all accepted")
	}
	fake.SetConnections(tun.ID, 2)
	got2, _ := c.GetTunnel(ctx, "a1", "tok", tun.ID)
	if got2.ActiveConnections() != 2 {
		t.Errorf("connections: %+v", got2)
	}
	if err := c.RotateTunnelSecret(ctx, "a1", "tok", tun.ID, "bmV3"); err != nil {
		t.Fatal(err)
	}
	if _, secret, _ := fake.Tunnel(tun.ID); secret != "bmV3" {
		t.Errorf("secret not rotated: %q", secret)
	}
	if err := c.DeleteTunnelConnections(ctx, "a1", "tok", tun.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteTunnel(ctx, "a1", "tok", tun.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.ListTunnels(ctx, "a1", "tok", "gw"); len(list) != 0 {
		t.Errorf("deleted tunnel listed: %v", list)
	}
	if got3, err := c.GetTunnel(ctx, "a1", "tok", tun.ID); err != nil || !got3.Deleted() {
		t.Errorf("deleted tunnel: %+v %v", got3, err)
	}
	if _, err := c.GetTunnel(ctx, "a1", "tok", "missing"); !cloudflare.IsNotFound(err) {
		t.Errorf("missing tunnel: %v", err)
	}
}

func TestZonesAndDNS(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	zid := fake.AddZone("example.com")
	fake.AddZone("other.org")
	fake.AddToken("dns", cftest.Token{Zones: []string{"example.com"}})
	z, err := c.FindZone(ctx, "dns", "", "example.com")
	if err != nil || z == nil || z.ID != zid {
		t.Fatalf("zone: %v %v", z, err)
	}
	if z, err := c.FindZone(ctx, "dns", "", "other.org"); err != nil || z != nil {
		t.Errorf("invisible zone: %v %v", z, err)
	}
	if n, err := c.CountZones(ctx, "dns", ""); err != nil || n != 1 {
		t.Errorf("count: %d %v", n, err)
	}
	rec, err := c.CreateDNSRecord(ctx, "dns", zid, cloudflare.DNSRecord{Type: "CNAME", Name: "a.example.com", Content: "x.cfargotunnel.com", Proxied: ptr.To(true), TTL: 1})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.ListDNSRecords(ctx, "dns", zid, "a.example.com", "CNAME")
	if err != nil || len(recs) != 1 {
		t.Fatalf("list: %v %v", recs, err)
	}
	if err := c.UpdateDNSRecord(ctx, "dns", zid, rec.ID, cloudflare.DNSRecord{Type: "CNAME", Name: "a.example.com", Content: "y.cfargotunnel.com"}); err != nil {
		t.Fatal(err)
	}
	if recs := fake.Records(zid); recs[0].Content != "y.cfargotunnel.com" {
		t.Errorf("update: %v", recs)
	}
	if err := c.DeleteDNSRecord(ctx, "dns", zid, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDNSRecord(ctx, "dns", zid, rec.ID); err != nil {
		t.Errorf("deleting a missing record: %v", err)
	}
}

func TestPagination(t *testing.T) {
	fake, c := setup(t)
	fake.AddToken("tok", cftest.Token{Account: "a1"})
	for i := 0; i < 250; i++ {
		fake.AddTunnel("a1", "t")
	}
	list, err := c.ListTunnels(context.Background(), "a1", "tok", "t")
	if err != nil || len(list) != 250 {
		t.Fatalf("got %d, %v", len(list), err)
	}
}

func TestRateLimit(t *testing.T) {
	fake, c := setup(t)
	fake.AddToken("tok", cftest.Token{Account: "a1"})
	fake.RateLimit(1)
	_, err := c.ListTunnels(context.Background(), "a1", "tok", "")
	if d, ok := cloudflare.RetryAfter(err); !ok || d != time.Second {
		t.Errorf("got %v %v", d, err)
	}
	// Without a Retry-After header the default backoff applies.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	_, err = cloudflare.New(srv.URL).ListTunnels(context.Background(), "a1", "tok", "")
	if d, ok := cloudflare.RetryAfter(err); !ok || d != cloudflare.DefaultRateLimitBackoff {
		t.Errorf("got %v %v", d, err)
	}
}

func TestAccessApps(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	fake.AddToken("tok", cftest.Token{Account: "a1"})
	app, err := c.CreateAccessApp(ctx, "a1", "tok", cloudflare.AccessApp{Name: "n", Domain: "db.example.com", Type: "self_hosted"})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := c.ListAccessApps(ctx, "a1", "tok")
	if err != nil || len(apps) != 1 {
		t.Fatalf("%v %v", apps, err)
	}
	if err := c.DeleteAccessApp(ctx, "a1", "tok", app.ID); err != nil {
		t.Fatal(err)
	}
	if len(fake.AccessApps()) != 0 {
		t.Error("not deleted")
	}
}
