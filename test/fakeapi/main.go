// Command fakeapi serves the in-memory fake Cloudflare API for end-to-end
// tests, with one account token that may manage tunnels and DNS in the
// zones listed in FAKE_ZONES. GET /_fake/state returns its state.
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare/cftest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	s := cftest.New()
	zones := strings.Split(env("FAKE_ZONES", "example.com"), ",")
	s.AddToken(env("FAKE_TOKEN", "e2e-token"), cftest.Token{Account: env("FAKE_ACCOUNT", "e2e-account"), Zones: zones})
	for _, z := range zones {
		s.AddZone(strings.TrimSpace(z))
	}
	addr := env("FAKE_ADDR", ":8080")
	log.Printf("fake Cloudflare API on %s, zones %v", addr, zones)
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
