package addon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/amendezsap/gardener-extension-shoot-addon-service/pkg/apis/config"
)

const (
	hookTestNamespace = "shoot--my-project--my-shoot"
	hookTestAddon     = "sample"
	hookTestMRName    = "addon-hook-sample-0"
	hookTestJobKey    = "hook-job-sample-0"
)

func newHookTestActuator(objs ...runtime.Object) *actuator {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = resourcesv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithRuntimeObjects(o)
	}
	return &actuator{client: builder.Build()}
}

func sampleHookJob() []byte {
	return []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: sample-hook-job\n")
}

// When no hash is recorded and no temp MR exists yet, the Job must actually be
// run (temp MR created) and the hash must NOT be recorded until GRM reports the
// MR terminal. This is the regression guard for the old "transition" shortcut
// that recorded the hash without running the Job, leaving whatever the Job was
// meant to create or populate missing.
func TestApplyShootHookJobs_RunsJobWhenNoHashRecorded(t *testing.T) {
	ctx := context.Background()
	a := newHookTestActuator()
	status := &config.AddonStatus{} // HookJobsCompleted == nil

	a.applyShootHookJobs(ctx, logr.Discard(), hookTestNamespace, hookTestAddon, [][]byte{sampleHookJob()}, status)

	mr := &resourcesv1alpha1.ManagedResource{}
	key := types.NamespacedName{Name: hookTestMRName, Namespace: hookTestNamespace}
	if err := a.client.Get(ctx, key, mr); err != nil {
		t.Fatalf("expected temp MR %q to be created so the hook Job runs, but Get failed: %v", hookTestMRName, err)
	}

	if _, recorded := status.HookJobsCompleted[hookTestJobKey]; recorded {
		t.Fatalf("hash must not be recorded until the Job completes; got %v", status.HookJobsCompleted)
	}
}

// Once the temp MR reports applied + healthy (Job finished), the hash is
// recorded and the temp MR is cleaned up.
func TestApplyShootHookJobs_RecordsHashWhenJobCompletes(t *testing.T) {
	ctx := context.Background()
	jobYAML := sampleHookJob()
	specHash := fmt.Sprintf("%x", sha256.Sum256(jobYAML))[:16]

	existing := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: hookTestMRName, Namespace: hookTestNamespace},
		Status: resourcesv1alpha1.ManagedResourceStatus{
			Conditions: []gardencorev1beta1.Condition{
				{Type: gardencorev1beta1.ConditionType(resourcesv1alpha1.ResourcesApplied), Status: gardencorev1beta1.ConditionTrue},
				{Type: gardencorev1beta1.ConditionType(resourcesv1alpha1.ResourcesHealthy), Status: gardencorev1beta1.ConditionTrue},
			},
		},
	}
	a := newHookTestActuator(existing)
	status := &config.AddonStatus{}

	a.applyShootHookJobs(ctx, logr.Discard(), hookTestNamespace, hookTestAddon, [][]byte{jobYAML}, status)

	if got := status.HookJobsCompleted[hookTestJobKey]; got != specHash {
		t.Fatalf("expected recorded hash %q after completion, got %q (full: %v)", specHash, got, status.HookJobsCompleted)
	}

	mr := &resourcesv1alpha1.ManagedResource{}
	err := a.client.Get(ctx, types.NamespacedName{Name: hookTestMRName, Namespace: hookTestNamespace}, mr)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected temp MR to be deleted after completion, got err=%v", err)
	}
}

// A matching recorded hash short-circuits: no temp MR is created and the hash
// is carried forward.
func TestApplyShootHookJobs_SkipsWhenHashMatches(t *testing.T) {
	ctx := context.Background()
	jobYAML := sampleHookJob()
	specHash := fmt.Sprintf("%x", sha256.Sum256(jobYAML))[:16]

	a := newHookTestActuator()
	status := &config.AddonStatus{HookJobsCompleted: map[string]string{hookTestJobKey: specHash}}

	a.applyShootHookJobs(ctx, logr.Discard(), hookTestNamespace, hookTestAddon, [][]byte{jobYAML}, status)

	if got := status.HookJobsCompleted[hookTestJobKey]; got != specHash {
		t.Fatalf("expected hash carried forward, got %q", got)
	}
	mr := &resourcesv1alpha1.ManagedResource{}
	err := a.client.Get(ctx, types.NamespacedName{Name: hookTestMRName, Namespace: hookTestNamespace}, mr)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no temp MR when hash matches, got err=%v", err)
	}
}

// mrObj is a small helper to build a ManagedResource for the fake client.
func mrObj(name string) *resourcesv1alpha1.ManagedResource {
	return &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hookTestNamespace},
	}
}

// deleteShootHookMRs must delete every temp hook MR for the addon
// (addon-hook-<addon>-<i>) and leave unrelated MRs — including the addon's own
// main MR and other addons' hook MRs — untouched. This is the cleanup that
// unblocks shoot deletion when a hibernated shoot left an un-finalizable hook MR.
func TestDeleteShootHookMRs_DeletesOnlyMatchingHookMRs(t *testing.T) {
	ctx := context.Background()
	a := newHookTestActuator(
		mrObj("addon-hook-sample-0"), // match
		mrObj("addon-hook-sample-1"), // match (multi-hook addon)
		mrObj("addon-sample"),        // addon's main MR — must survive
		mrObj("addon-hook-other-0"),  // different addon — must survive
		mrObj("fluent-bit"),          // unrelated — must survive
	)

	a.deleteShootHookMRs(ctx, logr.Discard(), hookTestNamespace, hookTestAddon)

	deleted := []string{"addon-hook-sample-0", "addon-hook-sample-1"}
	for _, name := range deleted {
		err := a.client.Get(ctx, types.NamespacedName{Name: name, Namespace: hookTestNamespace}, &resourcesv1alpha1.ManagedResource{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected hook MR %q deleted, got err=%v", name, err)
		}
	}

	survivors := []string{"addon-sample", "addon-hook-other-0", "fluent-bit"}
	for _, name := range survivors {
		if err := a.client.Get(ctx, types.NamespacedName{Name: name, Namespace: hookTestNamespace}, &resourcesv1alpha1.ManagedResource{}); err != nil {
			t.Fatalf("expected MR %q to survive hook cleanup, got err=%v", name, err)
		}
	}
}

// With no matching hook MRs present, cleanup is a no-op and does not error.
func TestDeleteShootHookMRs_NoMatchIsNoop(t *testing.T) {
	ctx := context.Background()
	a := newHookTestActuator(mrObj("addon-sample"))

	a.deleteShootHookMRs(ctx, logr.Discard(), hookTestNamespace, hookTestAddon)

	if err := a.client.Get(ctx, types.NamespacedName{Name: "addon-sample", Namespace: hookTestNamespace}, &resourcesv1alpha1.ManagedResource{}); err != nil {
		t.Fatalf("unrelated MR must survive, got err=%v", err)
	}
}

// Migrate must delete leftover temporary hook-Job MRs. Control-plane migration
// blocks on "wait until shoot managed resources have been deleted", so a
// lingering addon-hook-<addon>-<i> stalls the whole migration. This is the
// regression guard for that gap (Migrate previously deleted only the addon's
// main MR, not its temp hook MRs).
func TestMigrate_DeletesLeftoverHookMRs(t *testing.T) {
	ctx := context.Background()
	manifest := `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
defaultNamespace: managed-resources
addons:
  - name: sample
    chart:
      oci: oci://registry.example.com/charts/sample
      version: "1.0.0"
    enabled: true
    target: shoot
`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: hookTestNamespace},
		Data:       map[string]string{"manifest.yaml": manifest},
	}
	a := newHookTestActuator(cm, mrObj("addon-hook-sample-0"), mrObj("sample"))
	ex := &extensionsv1alpha1.Extension{ObjectMeta: metav1.ObjectMeta{Name: "shoot-addon-service", Namespace: hookTestNamespace}}

	if err := a.Migrate(ctx, logr.Discard(), ex); err != nil {
		t.Fatalf("Migrate returned error: %v", err)
	}

	err := a.client.Get(ctx, types.NamespacedName{Name: "addon-hook-sample-0", Namespace: hookTestNamespace}, &resourcesv1alpha1.ManagedResource{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected temp hook MR deleted by Migrate, got err=%v", err)
	}
}
