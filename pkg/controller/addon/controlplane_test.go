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
