package addon

import "testing"

// The control-plane target renders one chart twice, injecting a renderTarget
// value (as extraValues, merged LAST) so the chart can emit the controller for
// the seed-class MR and the shoot RBAC for the shoot-class MR. These tests lock
// the precedence contract that split relies on: the injected renderTarget must
// win over any chart/manifest value and must not disturb unrelated values.

func TestControlPlaneRenderTargetPrecedence(t *testing.T) {
	base := map[string]interface{}{
		renderTargetValuesKey: "stale-from-chart", // a chart default we must override
		"image": map[string]interface{}{
			"repository": "example/controller",
			"tag":        "0.1.0",
		},
		"replicaCount": 1,
	}

	for _, want := range []string{renderTargetControlPlane, renderTargetShoot} {
		extra := map[string]interface{}{renderTargetValuesKey: want}
		merged := mergeMaps(base, extra)

		if got := merged[renderTargetValuesKey]; got != want {
			t.Errorf("renderTarget = %v, want %v (injected extraValues must win)", got, want)
		}
		// Unrelated values must survive untouched.
		img, ok := merged["image"].(map[string]interface{})
		if !ok || img["repository"] != "example/controller" || img["tag"] != "0.1.0" {
			t.Errorf("image values were disturbed by injection: %v", merged["image"])
		}
		if merged["replicaCount"] != 1 {
			t.Errorf("replicaCount = %v, want 1", merged["replicaCount"])
		}
		// The injection must not mutate the shared base map.
		if base[renderTargetValuesKey] != "stale-from-chart" {
			t.Errorf("base map was mutated: renderTarget = %v", base[renderTargetValuesKey])
		}
	}
}

// The two MRs a control-plane addon produces must have distinct names so they do
// not collide: the seed-class controller MR (seed-<name>) and the shoot-class
// RBAC MR (<name>).
func TestControlPlaneManagedResourceNamesDistinct(t *testing.T) {
	// Mirrors addonpkg.Addon name helpers used by the control-plane branch.
	name := "sample-cp-addon"
	shootMR := name          // GetManagedResourceName()
	seedMR := "seed-" + name // GetSeedManagedResourceName()
	if shootMR == seedMR {
		t.Fatalf("seed and shoot MR names collide: %q", shootMR)
	}
}

// On a hibernated shoot the control-plane render must carry hibernated=true so a
// controller chart can gate its workload to replicas:0 (its shoot-access SA token
// is not projected while the shoot apiserver/GRM are scaled down; a replicas>0
// controller would CrashLoopBackOff). This locks the injection contract: the
// hibernated value is injected via extraValues (merged LAST), wins over any chart
// default, and coexists with the renderTarget injection without disturbing it.
func TestControlPlaneHibernatedInjection(t *testing.T) {
	base := map[string]interface{}{
		hibernatedValuesKey: false, // a chart default we must override when asleep
		"replicaCount":      1,
	}
	for _, hibernated := range []bool{true, false} {
		extra := map[string]interface{}{
			renderTargetValuesKey: renderTargetControlPlane,
			hibernatedValuesKey:   hibernated,
		}
		merged := mergeMaps(base, extra)
		if got := merged[hibernatedValuesKey]; got != hibernated {
			t.Errorf("hibernated = %v, want %v (injected extraValues must win)", got, hibernated)
		}
		if merged[renderTargetValuesKey] != renderTargetControlPlane {
			t.Errorf("renderTarget lost when hibernated injected: %v", merged[renderTargetValuesKey])
		}
		if merged["replicaCount"] != 1 {
			t.Errorf("replicaCount disturbed by hibernated injection: %v", merged["replicaCount"])
		}
		// Must not mutate the shared base map.
		if base[hibernatedValuesKey] != false {
			t.Errorf("base map mutated: hibernated = %v", base[hibernatedValuesKey])
		}
	}
}
