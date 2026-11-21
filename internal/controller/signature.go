package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
)

// Anyone who can edit a Gateway can also write the annotations the
// controller records on it. The ones that decide what the controller may
// delete or which Gateway keeps a hostname are therefore signed with a key
// that only the controller holds (a Secret in its own namespace), and are
// only trusted when the signature matches.

// signedAnnotations are covered by AnnotationSignature.
var signedAnnotations = []string{
	config.AnnotationTunnel, config.AnnotationPreviousTunnel,
	config.AnnotationHostnames, config.AnnotationDNSHostnames,
}

// signingKeyName is the Secret holding the signing key.
func signingKeyName(cfg *config.Config) string { return cfg.Instance + "-signing-key" }

const signingKeyField = "key"

// annotationSigner signs and verifies a Gateway's recorded annotations.
type annotationSigner struct {
	mu  sync.Mutex
	key []byte
}

// signingKey loads the key, creating it on first use.
func (r *GatewayReconciler) signingKey(ctx context.Context) ([]byte, error) {
	r.signer.mu.Lock()
	defer r.signer.mu.Unlock()
	if r.signer.key != nil {
		return r.signer.key, nil
	}
	key := types.NamespacedName{Namespace: r.Config.Namespace, Name: signingKeyName(r.Config)}
	for range 3 {
		s := &corev1.Secret{}
		err := r.reader().Get(ctx, key, s)
		if err == nil {
			k := s.Data[signingKeyField]
			if len(k) < 32 {
				return nil, fmt.Errorf("secret %s holds no signing key", key)
			}
			r.signer.key = k
			return k, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
		s = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: managedByLabels(r.Config)},
			Data:       map[string][]byte{signingKeyField: k},
		}
		if err := r.Create(ctx, s); err == nil {
			r.signer.key = k
			return k, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("cannot load secret %s", key)
}

func signAnnotations(key []byte, uid types.UID, ann map[string]string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s\x00", uid)
	for _, k := range signedAnnotations {
		fmt.Fprintf(m, "%s=%s\x00", k, ann[k])
	}
	return hex.EncodeToString(m.Sum(nil))
}

// recorded returns gw's signed annotations if their signature matches, and
// none otherwise. A Gateway without any of them has nothing to verify.
func (r *GatewayReconciler) recorded(ctx context.Context, gw *gwv1.Gateway) (map[string]string, error) {
	out := map[string]string{}
	empty := true
	for _, k := range signedAnnotations {
		if v := gw.Annotations[k]; v != "" {
			out[k] = v
			empty = false
		}
	}
	if empty {
		return out, nil
	}
	key, err := r.signingKey(ctx)
	if err != nil {
		return nil, err
	}
	want := signAnnotations(key, gw.UID, gw.Annotations)
	if !hmac.Equal([]byte(want), []byte(gw.Annotations[config.AnnotationSignature])) {
		return map[string]string{}, nil
	}
	return out, nil
}
