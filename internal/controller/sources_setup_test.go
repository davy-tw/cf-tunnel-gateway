package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

func setupSources(t *testing.T, mgr ctrl.Manager, c client.Client, scheme *runtime.Scheme, cfg *config.Config, rec events.EventRecorder) {
}
