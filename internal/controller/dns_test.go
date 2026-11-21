package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

func TestOwnershipTXTAttributes(t *testing.T) {
	cfg := &config.Config{
		DNSOwnerID:     "prod",
		DNSTXTKeys:     config.TXTKeys{Heritage: "managed-by", Owner: "cluster", Gateway: "gw"},
		DNSTXTHeritage: "tunnels",
	}
	s := &dnsSyncer{Config: cfg, Gateway: "ns/demo"}
	content := s.ownerContent()
	if want := "managed-by=tunnels,cluster=prod,gw=ns/demo"; content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
	if owner, gw, ok := s.txtOwner(`"` + content + `"`); !ok || owner != "prod" || gw != "ns/demo" {
		t.Errorf("parsed %q %q %v", owner, gw, ok)
	}
	for _, foreign := range []string{
		"heritage=cf-tunnel-gateway,owner=prod,gateway=ns/demo",
		"managed-by=external-dns,cluster=prod,gw=ns/demo",
		"cluster=prod,gw=ns/demo",
	} {
		if _, _, ok := s.txtOwner(foreign); ok {
			t.Errorf("%q counted as ours", foreign)
		}
	}
}

func TestTakeOverNeedsOwnersRecord(t *testing.T) {
	ref := translate.RouteRef{Kind: "HTTPRoute", Namespace: "a", Name: "r"}
	h := translate.Hostname{Name: "app.example.com", DNSParent: "a/new", DNSFrom: []string{"b/owner"}, Routes: []translate.RouteRef{ref}}
	s := &dnsSyncer{Gateway: "a/new", Published: map[string]map[string][]string{}}
	if s.mayTakeOver(h, "b/owner") {
		t.Error("took over without a record of the owner's routes")
	}
	s.Published["b/owner"] = map[string][]string{"app.example.com": {ref.String()}}
	if !s.mayTakeOver(h, "b/owner") {
		t.Error("did not take over when every route moved")
	}
}

func TestCacheOptionsKeepRecordsCached(t *testing.T) {
	o := CacheOptions(&config.Config{WatchNamespaces: []string{"a"}, Namespace: "ctrl"})
	for obj, b := range o.ByObject {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			if _, ok := b.Namespaces["ctrl"]; !ok {
				t.Errorf("ConfigMaps not cached in the controller namespace: %v", b.Namespaces)
			}
			if _, ok := b.Namespaces["a"]; !ok {
				t.Errorf("ConfigMaps not cached in watched namespace: %v", b.Namespaces)
			}
			return
		}
	}
	t.Error("no ConfigMap cache options")
}
