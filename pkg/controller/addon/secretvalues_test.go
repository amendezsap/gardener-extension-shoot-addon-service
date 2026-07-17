package addon

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	addonpkg "github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/addon"
)

// injectSecretValues must overlay a seed Secret's data onto the render values
// (so sensitive values come from a Secret, not the manifest ConfigMap) while
// preserving non-secret values already set under the key.
func TestInjectSecretValues(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "garden", Name: "sample-credentials"},
		Data: map[string][]byte{
			"username": []byte("user1"),
			"password": []byte("s3cr3t"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).WithObjects(secret).Build()
	a := &actuator{client: c}

	merged := map[string]interface{}{
		"credentials": map[string]interface{}{"secretName": "sample-credentials"}, // non-secret, from shootValues
		"image":       map[string]interface{}{"tag": "0.1.0"},
	}
	addon := &addonpkg.Addon{
		Name: "sample-addon",
		SecretValues: []addonpkg.SecretValueRef{
			{ValuesKey: "credentials", SeedSecretRef: addonpkg.SeedSecretRef{Name: "sample-credentials", Namespace: "garden"}},
		},
	}

	if err := a.injectSecretValues(context.Background(), merged, addon); err != nil {
		t.Fatalf("injectSecretValues: %v", err)
	}

	creds, _ := merged["credentials"].(map[string]interface{})
	if creds["username"] != "user1" || creds["password"] != "s3cr3t" {
		t.Errorf("secret data not merged into credentials: %v", creds)
	}
	if creds["secretName"] != "sample-credentials" {
		t.Errorf("non-secret secretName not preserved: %v", creds)
	}
	if merged["image"].(map[string]interface{})["tag"] != "0.1.0" {
		t.Errorf("unrelated values disturbed: %v", merged["image"])
	}
}

// Addons without SecretValues are a no-op and never touch the client.
func TestInjectSecretValues_NoEntriesIsNoop(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).Build()
	a := &actuator{client: c}
	merged := map[string]interface{}{"keep": "me"}
	if err := a.injectSecretValues(context.Background(), merged, &addonpkg.Addon{Name: "sample-addon"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged["keep"] != "me" {
		t.Errorf("no-op changed values: %v", merged)
	}
}

// A missing seed Secret is a hard error (fail closed rather than render without data).
func TestInjectSecretValues_MissingSecretErrors(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).Build()
	a := &actuator{client: c}
	addon := &addonpkg.Addon{
		Name: "sample-addon",
		SecretValues: []addonpkg.SecretValueRef{
			{ValuesKey: "credentials", SeedSecretRef: addonpkg.SeedSecretRef{Name: "missing", Namespace: "garden"}},
		},
	}
	if err := a.injectSecretValues(context.Background(), map[string]interface{}{}, addon); err == nil {
		t.Fatal("expected error for missing Secret, got nil")
	}
}
