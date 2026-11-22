package connector

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestParseParams(t *testing.T) {
	p, unknown, err := ParseParams(map[string]string{
		ParamReplicas:     "3",
		ParamNodeSelector: "pool: edge",
		ParamTolerations:  "- key: edge\n  operator: Exists",
		ParamResources:    "requests:\n  cpu: 50m",
		ParamAffinity:     "podAntiAffinity:\n  preferredDuringSchedulingIgnoredDuringExecution: []",
		ParamLogLevel:     "debug",
		"other":           "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Replicas != 3 || p.NodeSelector["pool"] != "edge" || len(p.Tolerations) != 1 || p.Resources.Requests.Cpu().String() != "50m" ||
		p.LogLevel != "debug" || p.Affinity.PodAntiAffinity == nil || len(unknown) != 1 {
		t.Errorf("%+v %v", p, unknown)
	}
	for k, v := range map[string]string{
		ParamReplicas: "0", ParamLogLevel: "trace", ParamDisable: "maybe", ParamNodeSelector: "[1",
		ParamResources: "requests: {cpu: 1}\nbogus: 1",
	} {
		if _, _, err := ParseParams(map[string]string{k: v}); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: %v", k, v, err)
		}
	}
}

func spec() *Spec {
	return &Spec{
		Instance: "cf-tunnel-gateway", Namespace: "ns", Gateway: "gw", Image: "cloudflare/cloudflared:x", Token: "tok",
		Params:    DefaultParams(),
		PodLabels: map[string]string{"team": "a"},
		Owner:     metav1.OwnerReference{Kind: "Gateway", Name: "gw"},
	}
}

func TestDeployment(t *testing.T) {
	s := spec()
	d := s.Deployment()
	if d.Name != "gw-cloudflared" || *d.Spec.Replicas != 1 || d.Spec.Template.Labels["team"] != "a" {
		t.Errorf("%+v", d.ObjectMeta)
	}
	if _, ok := d.Spec.Selector.MatchLabels["team"]; ok {
		t.Error("infrastructure labels must stay out of the selector")
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.EnvFrom[0].SecretRef.Name != "gw-cloudflared-token" || strings.Join(c.Args, " ") != "tunnel run" {
		t.Errorf("container: %+v", c)
	}
	psc := d.Spec.Template.Spec.SecurityContext
	if !*psc.RunAsNonRoot || psc.Sysctls[0].Name != "net.ipv4.ping_group_range" || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("hardening")
	}
	if d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Error("max unavailable")
	}
	h := d.Spec.Template.Annotations[AnnotationTokenHash]
	s.Token = "new"
	if s.Deployment().Spec.Template.Annotations[AnnotationTokenHash] == h {
		t.Error("a new token must roll the pods")
	}
	if s.PodDisruptionBudget() != nil {
		t.Error("no PDB with one replica")
	}
	s.Params.Replicas = 2
	if s.PodDisruptionBudget() == nil {
		t.Error("PDB with two replicas")
	}
	s.OmitReplicas = true
	if s.Deployment().Spec.Replicas != nil {
		t.Error("replicas left to the HPA")
	}
}

func TestAffinityMerge(t *testing.T) {
	a := mergeAffinity(&corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 1}},
	}})
	if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil || len(a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("%+v", a.NodeAffinity)
	}
}

func TestLabelValue(t *testing.T) {
	long := strings.Repeat("a", 80)
	if v := LabelValue(long); len(v) > 63 || v == LabelValue(long+"b") {
		t.Errorf("%q", v)
	}
}

func TestNamesFitKubernetesLimits(t *testing.T) {
	if got := DeploymentName("demo"); got != "demo-cloudflared" {
		t.Errorf("DeploymentName = %q", got)
	}
	if got := SecretName("demo"); got != "demo-cloudflared-token" {
		t.Errorf("SecretName = %q", got)
	}
	long := strings.Repeat("a", 240)
	other := strings.Repeat("a", 239) + "b"
	for _, f := range []func(string) string{DeploymentName, SecretName} {
		a, b := f(long), f(other)
		if len(a) > 253 || len(b) > 253 {
			t.Errorf("names too long: %d, %d", len(a), len(b))
		}
		if a == b {
			t.Errorf("names of different Gateways collide: %q", a)
		}
		if errs := validation.IsDNS1123Subdomain(a); len(errs) > 0 {
			t.Errorf("%q: %v", a, errs)
		}
	}
	if n := len(DeploymentName(long)); n > maxDeploymentName {
		t.Errorf("Deployment name leaves no room for Pod names: %d", n)
	}
}
