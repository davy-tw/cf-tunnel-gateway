package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{EnvNamespace: "cf-tunnel-gateway-system"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ControllerName != DefaultControllerName || c.GatewayClass != DefaultInstance || c.IngressClass != DefaultInstance ||
		c.LoadBalancerClass != DefaultInstance || c.SharedName != DefaultInstance {
		t.Errorf("names: %+v", c)
	}
	if c.SharedScope != ScopeNamespace || c.SharedNamespace != "cf-tunnel-gateway-system" || !c.IngressEnabled || !c.ServiceEnabled {
		t.Errorf("shared: %+v", c)
	}
	if c.DNSPolicy != DNSPolicySync || c.DNSOwnerID != DefaultInstance || c.DNSTXTPrefix != "_cf-tunnel-gateway" {
		t.Errorf("dns: %+v", c)
	}
	if c.SwitchTimeout != 5*time.Minute || c.SwitchGrace != 30*time.Second || c.TCPMode || c.AccessApps || c.DenyUnlistedHostnames {
		t.Errorf("misc: %+v", c)
	}
	if c.APIURL != "https://api.cloudflare.com/client/v4" || c.HasAccountCredentials() {
		t.Errorf("api: %+v", c)
	}
}

func TestInstanceDerivesNames(t *testing.T) {
	c, err := Load(env(map[string]string{EnvInstance: "team-a", EnvBaseDomain: "Apps.Example.com.", EnvWatchNamespaces: "a, b,,"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ControllerName != DefaultControllerName+"-team-a" || c.GatewayClass != "team-a" || c.DNSOwnerID != "team-a" || c.SharedName != "team-a" {
		t.Errorf("%+v", c)
	}
	if c.BaseDomain != "apps.example.com" || strings.Join(c.WatchNamespaces, ",") != "a,b" {
		t.Errorf("%+v", c)
	}
}

func TestInvalid(t *testing.T) {
	for name, m := range map[string]map[string]string{
		"scope":       {EnvSharedScope: "global"},
		"cluster ns":  {EnvSharedScope: ScopeCluster},
		"template":    {EnvHostnameTemplate: "x"},
		"policy":      {EnvDNSPolicy: "delete"},
		"bool":        {EnvTCPMode: "maybe"},
		"duration":    {EnvSwitchTimeout: "soon"},
		"half creds":  {EnvAccountID: "abc"},
		"lb class":    {EnvLoadBalancerClass: "a b"},
		"instance":    {EnvInstance: "A_B"},
		"txt prefix":  {EnvDNSTXTPrefix: "a.b"},
		"base domain": {EnvBaseDomain: "localhost"},
		"allowlist":   {EnvHostnameDefault: "maybe"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDenyAndCredentials(t *testing.T) {
	c, err := Load(env(map[string]string{EnvHostnameDefault: "deny", EnvAccountID: " acc ", EnvAPIToken: "tok", EnvAPIURL: "http://x/"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.DenyUnlistedHostnames || !c.HasAccountCredentials() || c.AccountID != "acc" || c.APIURL != "http://x" {
		t.Errorf("%+v", c)
	}
}
