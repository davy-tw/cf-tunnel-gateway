package controller

import (
	"testing"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
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
