package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSecretReconciler_PreservesUnownedNameCollision(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, ownership := range []string{"unmanaged", "labels-only", "other-owner", "previous-uid", "other-kind", "other-name", "not-controller"} {
			t.Run(fmt.Sprintf("enabled=%t/%s", enabled, ownership), func(t *testing.T) {
				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: "credentials", Namespace: "default", UID: "current-owner-uid",
						Labels: map[string]string{AnnotationSecretEnabled: fmt.Sprint(enabled)},
					},
					Data: map[string][]byte{"key": []byte("super-secret-value-12345678")},
				}
				collision := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: "credentials-kloak", Namespace: "default", UID: "collision-uid",
						Labels: map[string]string{
							"getkloak.io/managed": "true",
							"getkloak.io/owner":   "credentials",
						},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "v1", Kind: "Secret", Name: secret.Name,
							UID: secret.UID, Controller: ptr(true),
						}},
					},
					Data: map[string][]byte{"key": []byte("unrelated-credential-value")},
				}
				switch ownership {
				case "unmanaged":
					collision.Labels = nil
					collision.OwnerReferences = nil
				case "labels-only":
					collision.OwnerReferences = nil
				case "other-owner":
					collision.OwnerReferences[0].Name = "other-secret"
					collision.OwnerReferences[0].UID = "other-owner-uid"
				case "previous-uid":
					collision.OwnerReferences[0].UID = "previous-owner-uid"
				case "other-kind":
					collision.OwnerReferences[0].Kind = "ConfigMap"
				case "other-name":
					collision.OwnerReferences[0].Name = "other-secret"
				case "not-controller":
					collision.OwnerReferences[0].Controller = ptr(false)
				}
				r, c := newSecretReconciler(secret, collision)
				ctx := context.Background()
				key := types.NamespacedName{Namespace: collision.Namespace, Name: collision.Name}
				before := &corev1.Secret{}
				if err := c.Get(ctx, key, before); err != nil {
					t.Fatal(err)
				}
				_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
					Namespace: secret.Namespace, Name: secret.Name,
				}})
				if enabled && err == nil {
					t.Error("expected a name collision error instead of adopting the existing secret")
				}
				if !enabled && err != nil {
					t.Errorf("unrelated secret should be ignored during cleanup: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), string(collision.Data["key"])) {
					t.Error("error disclosed the existing secret value")
				}
				after := &corev1.Secret{}
				if err := c.Get(ctx, key, after); err != nil {
					t.Fatalf("unrelated secret was deleted: %v", err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Error("unrelated secret was modified")
				}
			})
		}
	}
}

func TestSecretReconciler_NestedEnabledSecret(t *testing.T) {
	// An enabled secret may legitimately end in -kloak. Its real value must
	// survive reconciliation of an unrelated, disabled secret with the base name.
	base := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "credentials", Namespace: "default", UID: "base-uid",
	}}
	nested := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "credentials-kloak", Namespace: "default", UID: "nested-uid",
			Labels: map[string]string{AnnotationSecretEnabled: "true"},
		},
		Data: map[string][]byte{"key": []byte("super-secret-value-12345678")},
	}
	r, c := newSecretReconciler(base, nested)
	ctx := context.Background()
	for _, name := range []string{nested.Name, base.Name, nested.Name + ShadowSecretSuffix} {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
			Name: name, Namespace: "default",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{nested.Name, nested.Name + ShadowSecretSuffix} {
		var got corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &got); err != nil {
			if apierrors.IsNotFound(err) {
				t.Fatalf("secret %s was deleted by unrelated reconciliation", name)
			}
			t.Fatal(err)
		}
	}
}

func TestSecretReconciler_ShadowCleanupErrors(t *testing.T) {
	for _, scenario := range []string{"read-error", "already-deleted", "changed-after-read"} {
		t.Run(scenario, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "credentials", Namespace: "default", UID: "owner-uid",
			}}
			shadow := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "credentials-kloak", Namespace: "default", UID: "shadow-uid",
				Labels: map[string]string{"getkloak.io/managed": "true"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Secret", Name: secret.Name,
					UID: secret.UID, Controller: ptr(true),
				}},
			}}
			r, c := newSecretReconciler(secret, shadow)
			readErr := errors.New("temporary API read failure")
			r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if scenario == "read-error" && key.Name == shadow.Name {
						return readErr
					}
					return cl.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if scenario == "already-deleted" {
						if err := cl.Delete(ctx, obj); err != nil {
							return err
						}
					} else if scenario == "changed-after-read" {
						// Simulate another actor changing ownership while the
						// reconciler still holds the old resource version.
						current := &corev1.Secret{}
						if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
							return err
						}
						current.OwnerReferences = nil
						if err := cl.Update(ctx, current); err != nil {
							return err
						}
					}
					return cl.Delete(ctx, obj, opts...)
				},
			})
			ctx := context.Background()
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: secret.Namespace, Name: secret.Name,
			}})
			switch scenario {
			case "read-error":
				if !errors.Is(err, readErr) {
					t.Fatalf("expected retryable read error, got %v", err)
				}
			case "already-deleted":
				if err != nil {
					t.Fatalf("cleanup should tolerate a concurrent deletion: %v", err)
				}
				return
			case "changed-after-read":
				if !apierrors.IsConflict(err) {
					t.Fatalf("expected conflict on stale deletion, got %v", err)
				}
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(shadow), &corev1.Secret{}); err != nil {
				t.Fatalf("shadow should be preserved: %v", err)
			}
		})
	}
}
