package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestServicePortsChanged(t *testing.T) {
	old := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
	status := old.DeepCopy()
	status.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "x"}}
	proto := old.DeepCopy()
	proto.Spec.Ports[0].AppProtocol = ptr.To("https")
	if servicePortsChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: status}) {
		t.Error("a status change passed")
	}
	if !servicePortsChanged.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: proto}) {
		t.Error("an appProtocol change did not pass")
	}
}
