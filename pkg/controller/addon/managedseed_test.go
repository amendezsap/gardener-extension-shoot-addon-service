package addon

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func seedScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

// A managed seed is a Gardener shoot, so its cluster has kube-system/shoot-info.
func TestCheckManagedSeedFromCluster_ShootInfoPresent(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: "shoot-info"}}
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).WithObjects(cm).Build()

	isManaged, authoritative := checkManagedSeedFromCluster(context.Background(), logr.Discard(), c, "managed-seed-a")
	if !isManaged || !authoritative {
		t.Fatalf("present shoot-info => managed+authoritative, got (%v,%v)", isManaged, authoritative)
	}
}

// A raw runtime seed (where gardener-operator runs) has no shoot-info.
func TestCheckManagedSeedFromCluster_ShootInfoAbsent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).Build()

	isManaged, authoritative := checkManagedSeedFromCluster(context.Background(), logr.Discard(), c, "raw-runtime-seed")
	if isManaged || !authoritative {
		t.Fatalf("absent shoot-info => not-managed+authoritative, got (%v,%v)", isManaged, authoritative)
	}
}

// A non-NotFound error (transient/RBAC) is not authoritative — callers must not
// cache it.
func TestCheckManagedSeedFromCluster_ReadErrorNotAuthoritative(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(seedScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("api down")
		},
	}).Build()

	isManaged, authoritative := checkManagedSeedFromCluster(context.Background(), logr.Discard(), c, "managed-seed-a")
	if isManaged || authoritative {
		t.Fatalf("transient error => not authoritative, got (%v,%v)", isManaged, authoritative)
	}
}

// isManagedSeed caches an authoritative result, but must NOT cache a
// non-authoritative one, so detection recovers on a later reconcile. This is the
// regression guard for the bug where a managed seed was permanently
// misclassified and spuriously deployed seed-class addons.
func TestIsManagedSeed_DoesNotCacheNonAuthoritative(t *testing.T) {
	a := &actuator{
		checkManagedSeedFn: func(_ context.Context, _ logr.Logger, _ string) (bool, bool) {
			return false, false // could not determine
		},
	}

	if a.isManagedSeed(context.Background(), logr.Discard(), "managed-seed-a") {
		t.Fatal("expected false when status is undetermined")
	}
	a.mu.Lock()
	cached := a.managedSeedChecked
	a.mu.Unlock()
	if cached {
		t.Fatal("must NOT cache a non-authoritative result")
	}

	// Detection recovers on a later reconcile.
	a.checkManagedSeedFn = func(_ context.Context, _ logr.Logger, _ string) (bool, bool) {
		return true, true
	}
	if !a.isManagedSeed(context.Background(), logr.Discard(), "managed-seed-a") {
		t.Fatal("expected detection to recover once determinable")
	}
	a.mu.Lock()
	cached, result := a.managedSeedChecked, a.managedSeedResult
	a.mu.Unlock()
	if !cached || !result {
		t.Fatalf("authoritative result should be cached (checked=%v result=%v)", cached, result)
	}
}
