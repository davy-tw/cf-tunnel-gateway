package controller

import (
	"context"
	"maps"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// Anyone who can edit a Gateway can also write, or restore an older copy
// of, the annotations the controller records on it. The values that
// decide what the controller may delete and which Gateway keeps a
// hostname are therefore kept in a ConfigMap per Gateway in the
// controller's own namespace, where tenants cannot write. The matching
// annotations on the Gateway only mirror them for people to read.

// recordedAnnotations are kept in the record.
var recordedAnnotations = []string{
	config.AnnotationTunnel, config.AnnotationPreviousTunnel,
	config.AnnotationHostnames, config.AnnotationDNSHostnames,
}

const (
	// labelRecord marks the record ConfigMaps.
	labelRecord        = config.Domain + "/record"
	recordUIDField     = "uid"
	recordGatewayField = "gateway"
)

// recordField is an annotation's key in the record.
func recordField(annotation string) string {
	return strings.TrimPrefix(annotation, config.Domain+"/")
}

// recordName is the ConfigMap holding gw's record.
func recordName(cfg *config.Config, uid types.UID) string {
	return cfg.Instance + "-gateway-" + string(uid)
}

func recordLabels(cfg *config.Config) map[string]string {
	l := managedByLabels(cfg)
	l[labelRecord] = "gateway"
	return l
}

// recordValues returns the recorded annotations in a record ConfigMap.
func recordValues(cm *corev1.ConfigMap) map[string]string {
	out := map[string]string{}
	for _, a := range recordedAnnotations {
		if v := cm.Data[recordField(a)]; v != "" {
			out[a] = v
		}
	}
	return out
}

// recorded returns what the controller recorded for gw.
func (r *GatewayReconciler) recorded(ctx context.Context, gw *gwv1.Gateway) (map[string]string, error) {
	cm := &corev1.ConfigMap{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: r.Config.Namespace, Name: recordName(r.Config, gw.UID)}, cm)
	if apierrors.IsNotFound(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if cm.Data[recordUIDField] != string(gw.UID) {
		return map[string]string{}, nil
	}
	return recordValues(cm), nil
}

// records returns every Gateway record of this installation, by Gateway
// UID, from the cache.
func (r *GatewayReconciler) records(ctx context.Context) (map[types.UID]map[string]string, error) {
	return r.listRecords(ctx, r.Client)
}

func (r *GatewayReconciler) listRecords(ctx context.Context, c client.Reader) (map[types.UID]map[string]string, error) {
	var list corev1.ConfigMapList
	if err := c.List(ctx, &list, client.InNamespace(r.Config.Namespace), client.MatchingLabels(recordLabels(r.Config))); err != nil {
		return nil, err
	}
	out := map[types.UID]map[string]string{}
	for i := range list.Items {
		cm := &list.Items[i]
		uid := types.UID(cm.Data[recordUIDField])
		if uid == "" || cm.Name != recordName(r.Config, uid) {
			continue
		}
		out[uid] = recordValues(cm)
	}
	return out, nil
}

// writeRecord stores rec as gw's record; an empty record is deleted.
func (r *GatewayReconciler) writeRecord(ctx context.Context, gw *gwv1.Gateway, rec map[string]string) error {
	data := map[string]string{recordUIDField: string(gw.UID), recordGatewayField: gatewayKey(gw)}
	empty := true
	for _, a := range recordedAnnotations {
		if v := rec[a]; v != "" {
			data[recordField(a)] = v
			empty = false
		}
	}
	if empty {
		return r.deleteRecord(ctx, gw)
	}
	key := types.NamespacedName{Namespace: r.Config.Namespace, Name: recordName(r.Config, gw.UID)}
	cm := &corev1.ConfigMap{}
	err := r.reader().Get(ctx, key, cm)
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: recordLabels(r.Config)},
			Data:       data,
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	if maps.Equal(cm.Data, data) && maps.Equal(cm.Labels, recordLabels(r.Config)) {
		return nil
	}
	cm.Data = data
	cm.Labels = recordLabels(r.Config)
	return r.Update(ctx, cm)
}

// deleteRecord removes gw's record.
func (r *GatewayReconciler) deleteRecord(ctx context.Context, gw *gwv1.Gateway) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: recordName(r.Config, gw.UID)}}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}

// sweepRecords deletes records whose Gateway is gone (its finalizer was
// removed by hand, or a restore gave it a new UID), at start and then
// every refreshEvery. It runs on the leader only.
func (r *GatewayReconciler) sweepRecords(ctx context.Context) error {
	for {
		if err := r.sweepRecordsOnce(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "removing stale Gateway records")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(refreshEvery):
		}
	}
}

func (r *GatewayReconciler) sweepRecordsOnce(ctx context.Context) error {
	// Records first: a record is written only after its Gateway exists,
	// so every Gateway with a record listed here is in the Gateway list.
	recs, err := r.listRecords(ctx, r.reader())
	if err != nil || len(recs) == 0 {
		return err
	}
	var list gwv1.GatewayList
	if err := r.reader().List(ctx, &list); err != nil {
		return err
	}
	live := map[types.UID]bool{}
	for i := range list.Items {
		live[list.Items[i].UID] = true
	}
	for uid := range recs {
		if !live[uid] {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: recordName(r.Config, uid)}}
			if err := client.IgnoreNotFound(r.Delete(ctx, cm)); err != nil {
				return err
			}
		}
	}
	return nil
}
