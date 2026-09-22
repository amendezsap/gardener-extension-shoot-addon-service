package addon

import "testing"

// The feature: a shoot's metadata.labels are exposed to shootValues templates as
// .ShootLabels, so addons can derive per-shoot config from a label
// (e.g. {{ index .ShootLabels "cloudability.sap/env" }}). These tests lock the
// expansion behavior and the nil-safety contract that newTemplateData guarantees.

func TestExpandValueShootLabels(t *testing.T) {
	data := testData()
	data.ShootLabels = map[string]string{
		"cloudability.sap/env":       "prod",
		"simplekey":                  "v1",
		"landscape.sapcloud.io/name": "cre",
	}

	tests := []struct {
		name     string
		input    interface{}
		expected interface{}
	}{
		// Label keys with "/" or "." require the index function.
		{"index dotted-slash key", `{{ index .ShootLabels "cloudability.sap/env" }}`, "prod"},
		{"index simple key", `{{ index .ShootLabels "simplekey" }}`, "v1"},
		// A key valid as a Go identifier can use dot access.
		{"dot access simple key", "{{ .ShootLabels.simplekey }}", "v1"},
		// Composed with other vars.
		{"combined", `cid-{{ .Project }}-{{ index .ShootLabels "cloudability.sap/env" }}`, "cid-my-project-prod"},
		// Missing key yields "" (Go template index on a map returns the zero value).
		{"missing key -> empty", `{{ index .ShootLabels "does-not-exist" }}`, ""},
		// Sprig default provides a fallback for an unset label.
		{"missing key with default", `{{ index .ShootLabels "does-not-exist" | default "fallback" }}`, "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandValue(tt.input, data)
			if got != tt.expected {
				t.Errorf("expandValue(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}

// newTemplateData must default ShootLabels to a non-nil empty map so that
// `index .ShootLabels "k"` on a label-less shoot (or a seed-class render where
// labels are not populated) evaluates to "" instead of failing on a nil map.
func TestNewTemplateDataShootLabelsNilSafe(t *testing.T) {
	// meta with nil labels (e.g. seed/managed-seed path, or a shoot with no labels).
	td := newTemplateData(&shootMetadata{Name: "s", Namespace: "garden-p"})
	if td.ShootLabels == nil {
		t.Fatal("newTemplateData left ShootLabels nil; index on it would fail")
	}
	if got := expandValue(`{{ index .ShootLabels "anything" }}`, td); got != "" {
		t.Errorf("index on empty ShootLabels = %v, want empty string", got)
	}

	// meta WITH labels must pass them through unchanged.
	td2 := newTemplateData(&shootMetadata{ShootLabels: map[string]string{"k": "val"}})
	if got := expandValue(`{{ index .ShootLabels "k" }}`, td2); got != "val" {
		t.Errorf("index on populated ShootLabels = %v, want val", got)
	}
}

// A .ShootLabels reference misused in a *values file* (not shootValues) must be
// caught by the unresolved-template guard, consistent with the other Gardener vars.
func TestUnresolvedGardenerTemplateCatchesShootLabels(t *testing.T) {
	if !unresolvedGardenerTemplate.MatchString(`{{ index .ShootLabels "x" }}`) {
		t.Error("unresolvedGardenerTemplate did not catch a .ShootLabels reference")
	}
	if !unresolvedGardenerTemplate.MatchString("{{ .ShootLabels.foo }}") {
		t.Error("unresolvedGardenerTemplate did not catch dot-access .ShootLabels")
	}
}
