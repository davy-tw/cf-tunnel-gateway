package controller

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare/cftest"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// A failed verification is retried after a minute; a successful one is
// reused for an hour.
func TestVerifyCachesFailuresBriefly(t *testing.T) {
	fake := cftest.New()
	fake.AddToken(testToken, cftest.Token{Account: testAccount, Zones: []string{"*"}})
	fake.AddZone(testZone)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	r := NewGatewayClassReconciler(nil, &config.Config{}, cloudflare.New(srv.URL+"/client/v4"))
	gc := &gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{UID: "class-uid", Generation: 1}}
	creds := &Credentials{AccountID: testAccount, AccountToken: testToken, DNSToken: testToken, SecretVersion: "1"}
	ctx := context.Background()
	age := func(d time.Duration) {
		v := r.verified[gc.UID]
		v.at = v.at.Add(-d)
		r.verified[gc.UID] = v
	}
	listTunnels := func() int {
		n := 0
		for _, c := range fake.Calls() {
			if c == "GET /accounts/"+testAccount+"/cfd_tunnel" {
				n++
			}
		}
		return n
	}

	fake.FailNext("GET /accounts/"+testAccount+"/cfd_tunnel", 1)
	if err := r.verify(ctx, gc, creds); err == nil {
		t.Fatal("verify succeeded despite a failing Cloudflare API")
	}
	if err := r.verify(ctx, gc, creds); err == nil {
		t.Fatal("a failure was not remembered for a while")
	}
	age(retryFailedEvery)
	if err := r.verify(ctx, gc, creds); err != nil {
		t.Fatalf("a failure was remembered for more than %s: %v", retryFailedEvery, err)
	}
	calls := listTunnels()
	age(retryFailedEvery)
	if err := r.verify(ctx, gc, creds); err != nil || listTunnels() != calls {
		t.Fatalf("a success was not reused: err=%v, calls %d -> %d", err, calls, listTunnels())
	}
}
