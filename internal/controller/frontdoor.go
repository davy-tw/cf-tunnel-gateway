package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/connector"
)

// FrontDoorCleanup removes what a turned-off front door of this
// installation generated: its routes and dedicated Gateways, and (for
// Services of its loadBalancerClass) the finalizer and load balancer
// status. It runs once, as the leader, at startup.
type FrontDoorCleanup struct {
	Client   client.Client
	Config   *config.Config
	Features *Features
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (c *FrontDoorCleanup) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (c *FrontDoorCleanup) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("front-door-cleanup")
	var kinds []string
	if !c.Config.IngressEnabled {
		kinds = append(kinds, config.SourceIngress)
	}
	if !c.Config.ServiceEnabled {
		kinds = append(kinds, config.SourceService)
	}
	for _, kind := range kinds {
		lists := []client.ObjectList{&gwv1.HTTPRouteList{}, &gwv1.GatewayList{}}
		if c.Features.TCPRoute() {
			lists = append(lists, &gwv1.TCPRouteList{})
		}
		for _, l := range lists {
			if err := c.Client.List(ctx, l, client.MatchingLabels{config.LabelSourceKind: kind, connector.LabelManagedBy: c.Config.Instance}); err != nil {
				logger.Error(err, "listing generated objects", "source", kind)
				continue
			}
			items, _ := listItems(l)
			for _, o := range items {
				if err := c.Client.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
					logger.Error(err, "deleting", "object", o.GetNamespace()+"/"+o.GetName())
				} else {
					logger.Info("deleted an object of a turned-off front door", "source", kind, "object", o.GetNamespace()+"/"+o.GetName())
				}
			}
		}
	}
	if !c.Config.IngressEnabled && !c.Config.ServiceEnabled {
		// Nothing generates routes any more, so shared Gateways have no
		// users (with a front door on, the shared Gateway controller
		// deletes unused ones).
		var gws gwv1.GatewayList
		if err := c.Client.List(ctx, &gws, client.MatchingLabels(managedByLabels(c.Config))); err == nil {
			for i := range gws.Items {
				if gws.Items[i].Annotations[config.AnnotationManaged] == config.ManagedShared {
					if err := c.Client.Delete(ctx, &gws.Items[i]); client.IgnoreNotFound(err) != nil {
						logger.Error(err, "deleting shared Gateway", "gateway", gws.Items[i].Namespace+"/"+gws.Items[i].Name)
					}
				}
			}
		}
	}
	if !c.Config.ServiceEnabled {
		var list corev1.ServiceList
		if err := c.Client.List(ctx, &list); err != nil {
			logger.Error(err, "listing Services")
			return nil
		}
		for i := range list.Items {
			svc := &list.Items[i]
			if !controllerutil.ContainsFinalizer(svc, c.Config.Finalizer) ||
				svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != c.Config.LoadBalancerClass {
				continue
			}
			orig := svc.DeepCopy()
			if len(svc.Status.LoadBalancer.Ingress) > 0 {
				svc.Status.LoadBalancer.Ingress = nil
				if err := c.Client.Status().Patch(ctx, svc, client.MergeFrom(orig)); err != nil {
					logger.Error(err, "clearing Service status", "service", svc.Namespace+"/"+svc.Name)
				}
				orig = svc.DeepCopy()
			}
			controllerutil.RemoveFinalizer(svc, c.Config.Finalizer)
			if err := c.Client.Patch(ctx, svc, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "removing the finalizer", "service", svc.Namespace+"/"+svc.Name)
			}
		}
	}
	return nil
}

// CheckIngressClass logs an error when the IngressClass of this instance's
// name was shipped by another installation.
func CheckIngressClass(ctx context.Context, c client.Reader, cfg *config.Config) {
	ic := &networkingv1.IngressClass{}
	if err := c.Get(ctx, types.NamespacedName{Name: cfg.IngressClass}, ic); err != nil {
		return
	}
	if v := ic.Annotations[config.AnnotationInstance]; v != "" && v != cfg.Instance {
		log.FromContext(ctx).Error(nil, "the IngressClass of this instance's name belongs to another installation; give each installation its own instance name",
			"ingressClass", cfg.IngressClass, "installation", v, "instance", cfg.Instance)
	}
}
