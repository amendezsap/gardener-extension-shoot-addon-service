package addon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

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
