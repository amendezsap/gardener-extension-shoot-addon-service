package addon

import (
	"testing"
)

func TestExpandValue(t *testing.T) {
	meta := &shootMetadata{
		Name:                      "my-shoot",
		Namespace:                 "garden-my-project",
		Project:                   "my-project",
		ControlNamespace:          "shoot--my-project--my-shoot",
		Region:                    "us-east-1",
		ProviderType:              "aws",
		SeedName:                  "my-seed",
		ClusterRole:               "shoot",
		ManagedKubernetesProvider: "",
	}

	tests := []struct {
		name     string
		input    interface{}
		expected interface{}
	}{
		{
			name:     "Region",
			input:    "{{ .Region }}",
			expected: "us-east-1",
		},
		{
			name:     "SeedName",
			input:    "{{ .SeedName }}",
			expected: "my-seed",
		},
		{
			name:     "ShootName",
			input:    "{{ .ShootName }}",
			expected: "my-shoot",
		},
		{
			name:     "ShootNamespace",
			input:    "{{ .ShootNamespace }}",
			expected: "garden-my-project",
		},
		{
			name:     "Project",
			input:    "{{ .Project }}",
			expected: "my-project",
		},
		{
			name:     "ControlNamespace",
			input:    "{{ .ControlNamespace }}",
			expected: "shoot--my-project--my-shoot",
		},
		{
			name:     "ProviderType",
			input:    "{{ .ProviderType }}",
			expected: "aws",
		},
		{
			name:     "ClusterRole",
			input:    "{{ .ClusterRole }}",
			expected: "shoot",
		},
		{
			name:     "ManagedKubernetesProvider empty",
			input:    "{{ .ManagedKubernetesProvider }}",
			expected: "",
		},
		{
			name:     "Combined variables",
			input:    "env-{{ .Project }}-{{ .Region }}",
			expected: "env-my-project-us-east-1",
		},
		{
			name:     "Non-template string",
			input:    "literal value",
			expected: "literal value",
		},
		{
			name:     "Non-string type passed through",
			input:    42,
			expected: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandValue(tt.input, meta)
			if got != tt.expected {
				t.Errorf("expandValue() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExpandValueRuntime(t *testing.T) {
	meta := &shootMetadata{
		SeedName:                  "runtime-seed",
		ProviderType:              "gcp",
		ClusterRole:               "runtime",
		ManagedKubernetesProvider: "GKE",
	}

	if got := expandValue("{{ .ClusterRole }}", meta); got != "runtime" {
		t.Errorf("ClusterRole = %v, want runtime", got)
	}
	if got := expandValue("{{ .ManagedKubernetesProvider }}", meta); got != "GKE" {
		t.Errorf("ManagedKubernetesProvider = %v, want GKE", got)
	}
	if got := expandValue("{{ .ProviderType }}", meta); got != "gcp" {
		t.Errorf("ProviderType = %v, want gcp", got)
	}
}

func TestExpandValueManagedSeed(t *testing.T) {
	meta := &shootMetadata{
		Name:                      "managed-seed-1",
		ProviderType:              "aws",
		ClusterRole:               "managed-seed",
		ManagedKubernetesProvider: "",
	}

	if got := expandValue("{{ .ClusterRole }}", meta); got != "managed-seed" {
		t.Errorf("ClusterRole = %v, want managed-seed", got)
	}
	if got := expandValue("{{ .ManagedKubernetesProvider }}", meta); got != "" {
		t.Errorf("ManagedKubernetesProvider = %v, want empty", got)
	}
}

func TestExpandValueRecursive(t *testing.T) {
	meta := &shootMetadata{
		Region:  "us-east-1",
		Project: "my-project",
	}

	input := map[string]interface{}{
		"region": "{{ .Region }}",
		"nested": map[string]interface{}{
			"project": "{{ .Project }}",
		},
		"list": []interface{}{"{{ .Region }}", "literal"},
	}

	got := expandValue(input, meta)
	gotMap, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", got)
	}

	if gotMap["region"] != "us-east-1" {
		t.Errorf("region = %v, want us-east-1", gotMap["region"])
	}
	nested, ok := gotMap["nested"].(map[string]interface{})
	if !ok {
		t.Fatal("expected nested map")
	}
	if nested["project"] != "my-project" {
		t.Errorf("nested.project = %v, want my-project", nested["project"])
	}
	list, ok := gotMap["list"].([]interface{})
	if !ok {
		t.Fatal("expected list")
	}
	if list[0] != "us-east-1" {
		t.Errorf("list[0] = %v, want us-east-1", list[0])
	}
	if list[1] != "literal" {
		t.Errorf("list[1] = %v, want literal", list[1])
	}
}

func TestHasLabelPrefix(t *testing.T) {
	labels := map[string]string{
		"cloud.google.com/gke-nodepool":    "default",
		"kubernetes.io/os":                  "linux",
		"node.kubernetes.io/instance-type":  "n1-standard-2",
	}

	if !hasLabelPrefix(labels, "cloud.google.com/gke-") {
		t.Error("expected GKE label prefix to match")
	}
	if !hasLabelPrefix(labels, "kubernetes.io/") {
		t.Error("expected kubernetes.io/ prefix to match")
	}
	if hasLabelPrefix(labels, "eks.amazonaws.com/") {
		t.Error("expected EKS label prefix to NOT match")
	}
	if hasLabelPrefix(labels, "") {
		// Empty prefix matches all keys, this is expected behavior of strings.HasPrefix
		// but worth noting
	}
}

func TestHasLabel(t *testing.T) {
	labels := map[string]string{
		"node.openshift.io/os_id": "rhcos",
		"kubernetes.io/os":        "linux",
	}

	if !hasLabel(labels, "node.openshift.io/os_id") {
		t.Error("expected OpenShift label to be present")
	}
	if !hasLabel(labels, "kubernetes.io/os") {
		t.Error("expected kubernetes.io/os label to be present")
	}
	if hasLabel(labels, "missing.label") {
		t.Error("expected missing label to return false")
	}
}
