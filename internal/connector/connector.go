// Package connector builds the objects that run cloudflared for a Gateway:
// the connector token Secret, the Deployment and its PodDisruptionBudget.
package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// Names and keys.
const (
	AppName = "cloudflared"
	// TokenKey is the variable cloudflared reads the tunnel token from.
	TokenKey = "TUNNEL_TOKEN"
	// MetricsPort serves cloudflared's metrics and /ready.
	MetricsPort = 2000
	// UID is the non-root user of the official image.
	UID = 65532

	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelComponent = "app.kubernetes.io/component"
	LabelGateway   = config.Domain + "/gateway"

	ComponentConnector = "connector"
	ComponentToken     = "tunnel-token"

	AnnotationTokenHash = config.Domain + "/token-hash"
)

// DeploymentName is the connector Deployment (and PodDisruptionBudget) of
// a Gateway. It leaves room for the ReplicaSet and Pod name suffixes.
func DeploymentName(gateway string) string {
	return boundedName(gateway, "-cloudflared", maxDeploymentName)
}

// SecretName is the connector token Secret of a Gateway.
func SecretName(gateway string) string {
	return boundedName(gateway, "-cloudflared-token", maxObjectName)
}

const (
	maxObjectName = 253
	// A Pod is named <deployment>-<10 character hash>-<5 characters>.
	maxDeploymentName = maxObjectName - 17
)

// boundedName is base+suffix, with base shortened and a hash of it added
// when the result would be longer than max.
func boundedName(base, suffix string, max int) string {
	if len(base)+len(suffix) <= max {
		return base + suffix
	}
	h := sha256.Sum256([]byte(base))
	return strings.TrimRight(base[:max-len(suffix)-11], "-.") + "-" + hex.EncodeToString(h[:])[:10] + suffix
}

// LabelValue fits a name into a label value, with a hash when too long.
func LabelValue(name string) string {
	if len(name) <= 63 {
		return name
	}
	h := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:52], "-.") + "-" + hex.EncodeToString(h[:])[:10]
}

// SelectorLabels select a Gateway's connector pods. They never change.
func SelectorLabels(instance, gateway string) map[string]string {
	return map[string]string{LabelName: AppName, LabelManagedBy: instance, LabelGateway: LabelValue(gateway)}
}

// Params are the per-Gateway settings from the infrastructure parameters
// ConfigMap.
type Params struct {
	Disabled     bool
	Replicas     int32
	NodeSelector map[string]string
	Affinity     *corev1.Affinity
	Tolerations  []corev1.Toleration
	Resources    *corev1.ResourceRequirements
	LogLevel     string
}

// Parameter keys.
const (
	ParamDisable      = "disableConnector"
	ParamReplicas     = "replicas"
	ParamNodeSelector = "nodeSelector"
	ParamAffinity     = "affinity"
	ParamTolerations  = "tolerations"
	ParamResources    = "resources"
	ParamLogLevel     = "logLevel"
)

// MaxReplicas bounds connector replicas.
const MaxReplicas = 64

// DefaultParams are used without a parameters ConfigMap.
func DefaultParams() Params { return Params{Replicas: 1, LogLevel: "info"} }

// ParseParams reads a parameters ConfigMap's data. It returns the unknown
// keys (sorted) and an error naming the offending key.
func ParseParams(data map[string]string) (Params, []string, error) {
	p := DefaultParams()
	var unknown []string
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.TrimSpace(data[k])
		if v == "" {
			continue
		}
		var err error
		switch k {
		case ParamDisable:
			p.Disabled, err = strconv.ParseBool(v)
		case ParamReplicas:
			var n int64
			n, err = strconv.ParseInt(v, 10, 32)
			if err == nil && (n < 1 || n > MaxReplicas) {
				err = fmt.Errorf("must be from 1 to %d", MaxReplicas)
			}
			p.Replicas = int32(n)
		case ParamNodeSelector:
			err = yaml.UnmarshalStrict([]byte(v), &p.NodeSelector)
		case ParamAffinity:
			p.Affinity = &corev1.Affinity{}
			err = yaml.UnmarshalStrict([]byte(v), p.Affinity)
		case ParamTolerations:
			err = yaml.UnmarshalStrict([]byte(v), &p.Tolerations)
		case ParamResources:
			p.Resources = &corev1.ResourceRequirements{}
			err = yaml.UnmarshalStrict([]byte(v), p.Resources)
		case ParamLogLevel:
			switch v {
			case "debug", "info", "warn", "error", "fatal":
				p.LogLevel = v
			default:
				err = fmt.Errorf("must be debug, info, warn, error or fatal")
			}
		default:
			unknown = append(unknown, k)
			continue
		}
		if err != nil {
			return Params{}, nil, fmt.Errorf("key %q: %v", k, err)
		}
	}
	return p, unknown, nil
}

// Spec describes a Gateway's connector.
type Spec struct {
	Instance  string
	Namespace string
	Gateway   string
	Image     string
	Params    Params
	Token     string
	// PodLabels and PodAnnotations come from spec.infrastructure.
	PodLabels      map[string]string
	PodAnnotations map[string]string
	Owner          metav1.OwnerReference
	// OmitReplicas leaves replicas to a HorizontalPodAutoscaler.
	OmitReplicas bool
}

func (s *Spec) labels(component string) map[string]string {
	l := map[string]string{}
	for k, v := range s.PodLabels {
		l[k] = v
	}
	for k, v := range SelectorLabels(s.Instance, s.Gateway) {
		l[k] = v
	}
	l[LabelInstance] = LabelValue(s.Gateway)
	l[LabelComponent] = component
	return l
}

func (s *Spec) meta(name, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: s.labels(component), OwnerReferences: []metav1.OwnerReference{s.Owner}}
}

// TokenHash is a short hash of the token, used to roll pods on a change.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])[:16]
}

// Secret is the connector token Secret.
func (s *Spec) Secret() *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: s.meta(SecretName(s.Gateway), ComponentToken),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{TokenKey: []byte(s.Token)},
	}
	// The pod labels are for pods only.
	sec.Labels = map[string]string{LabelName: AppName, LabelManagedBy: s.Instance, LabelGateway: LabelValue(s.Gateway), LabelComponent: ComponentToken}
	return sec
}

func defaultAffinity() *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
				{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64", "arm64"}},
			},
		}}},
	}}
}

// mergeAffinity lays the user's affinity over the default: each kind the
// user sets replaces the default of that kind.
func mergeAffinity(user *corev1.Affinity) *corev1.Affinity {
	a := defaultAffinity()
	if user == nil {
		return a
	}
	if user.NodeAffinity != nil {
		a.NodeAffinity = user.NodeAffinity.DeepCopy()
		if a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = defaultAffinity().NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		}
	}
	a.PodAffinity = user.PodAffinity.DeepCopy()
	a.PodAntiAffinity = user.PodAntiAffinity.DeepCopy()
	return a
}

// Deployment runs cloudflared.
func (s *Spec) Deployment() *appsv1.Deployment {
	sel := SelectorLabels(s.Instance, s.Gateway)
	podMeta := metav1.ObjectMeta{Labels: s.labels(ComponentConnector), Annotations: map[string]string{}}
	for k, v := range s.PodAnnotations {
		podMeta.Annotations[k] = v
	}
	podMeta.Annotations[AnnotationTokenHash] = TokenHash(s.Token)
	res := corev1.ResourceRequirements{}
	if s.Params.Resources != nil {
		res = *s.Params.Resources
	}
	logLevel := s.Params.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}
	probe := func(failures, period int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(MetricsPort)}},
			PeriodSeconds:    period,
			FailureThreshold: failures,
			TimeoutSeconds:   3,
		}
	}
	d := &appsv1.Deployment{
		ObjectMeta: s.meta(DeploymentName(s.Gateway), ComponentConnector),
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromInt32(0)), MaxSurge: ptr.To(intstr.FromInt32(1)),
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: podMeta,
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(int64(UID)),
						RunAsGroup:     ptr.To(int64(UID)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						Sysctls:        []corev1.Sysctl{{Name: "net.ipv4.ping_group_range", Value: fmt.Sprintf("%d %d", UID, UID)}},
					},
					Affinity:     mergeAffinity(s.Params.Affinity),
					NodeSelector: s.Params.NodeSelector,
					Tolerations:  s.Params.Tolerations,
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.ScheduleAnyway,
						LabelSelector: &metav1.LabelSelector{MatchLabels: sel},
					}},
					Containers: []corev1.Container{{
						Name:  AppName,
						Image: s.Image,
						Args:  []string{"tunnel", "run"},
						EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(s.Gateway)}, Optional: ptr.To(false),
						}}},
						Env: []corev1.EnvVar{
							{Name: "NO_AUTOUPDATE", Value: "true"},
							{Name: "TUNNEL_METRICS", Value: fmt.Sprintf("0.0.0.0:%d", MetricsPort)},
							{Name: "TUNNEL_LOGLEVEL", Value: logLevel},
						},
						Ports:          []corev1.ContainerPort{{Name: "metrics", ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP}},
						StartupProbe:   probe(10, 5),
						LivenessProbe:  probe(3, 10),
						ReadinessProbe: probe(1, 5),
						Resources:      res,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	if !s.OmitReplicas {
		d.Spec.Replicas = ptr.To(s.Params.Replicas)
	}
	return d
}

// PodDisruptionBudget keeps one connector up during voluntary
// disruptions; nil with a single replica, where it could only block drains.
func (s *Spec) PodDisruptionBudget() *policyv1.PodDisruptionBudget {
	if s.Params.Replicas < 2 && !s.OmitReplicas {
		return nil
	}
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: s.meta(DeploymentName(s.Gateway), ComponentConnector),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: SelectorLabels(s.Instance, s.Gateway)},
		},
	}
}
