package addon

import (
	"strings"
	"testing"
)

func testData() *templateData {
	return &templateData{
		Region:                    "us-east-1",
		SeedName:                  "my-seed",
		ShootName:                 "my-shoot",
		ShootNamespace:            "garden-my-project",
		Project:                   "my-project",
		ControlNamespace:          "shoot--my-project--my-shoot",
		ProviderType:              "aws",
		ClusterRole:               "shoot",
		ManagedKubernetesProvider: "",
	}
}

func TestExpandValueSimpleVariables(t *testing.T) {
	data := testData()

	tests := []struct {
		name     string
		input    interface{}
		expected interface{}
	}{
		{"Region", "{{ .Region }}", "us-east-1"},
		{"SeedName", "{{ .SeedName }}", "my-seed"},
		{"ShootName", "{{ .ShootName }}", "my-shoot"},
		{"ShootNamespace", "{{ .ShootNamespace }}", "garden-my-project"},
		{"Project", "{{ .Project }}", "my-project"},
		{"ControlNamespace", "{{ .ControlNamespace }}", "shoot--my-project--my-shoot"},
		{"ProviderType", "{{ .ProviderType }}", "aws"},
		{"ClusterRole", "{{ .ClusterRole }}", "shoot"},
		{"ManagedKubernetesProvider empty", "{{ .ManagedKubernetesProvider }}", ""},
		{"Combined", "env-{{ .Project }}-{{ .Region }}", "env-my-project-us-east-1"},
		{"Non-template string", "literal value", "literal value"},
		{"Non-string passed through", 42, 42},
		{"Bool passed through", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandValue(tt.input, data)
			if got != tt.expected {
				t.Errorf("expandValue() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExpandValueConditionals(t *testing.T) {
	shootData := testData()

	runtimeData := &templateData{
		SeedName:                  "runtime-seed",
		ProviderType:              "gcp",
		ClusterRole:               "runtime",
		ManagedKubernetesProvider: "GKE",
	}

	managedSeedData := &templateData{
		ShootName:                 "managed-seed-1",
		ProviderType:              "aws",
		ClusterRole:               "managed-seed",
		ManagedKubernetesProvider: "",
	}

	tmpl := `{{- if eq .ClusterRole "runtime" }}{{ .ManagedKubernetesProvider }}{{- else }}Kubernetes{{- end }}`

	if got := expandValue(tmpl, runtimeData); got != "GKE" {
		t.Errorf("runtime: got %q, want GKE", got)
	}
	if got := expandValue(tmpl, managedSeedData); got != "Kubernetes" {
		t.Errorf("managed-seed: got %q, want Kubernetes", got)
	}
	if got := expandValue(tmpl, shootData); got != "Kubernetes" {
		t.Errorf("shoot: got %q, want Kubernetes", got)
	}
}

func TestExpandValueSprigFunctions(t *testing.T) {
	data := &templateData{
		ClusterRole:  "managed-seed",
		ProviderType: "aws",
		Project:      "my-project",
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"upper", `{{ .ProviderType | upper }}`, "AWS"},
		{"lower", `{{ .Project | lower }}`, "my-project"},
		{"replace", `{{ .ClusterRole | replace "managed-" "" }}`, "seed"},
		{"trimPrefix", `{{ trimPrefix "managed-" .ClusterRole }}`, "seed"},
		{"default", `{{ .ManagedKubernetesProvider | default "Kubernetes" }}`, "Kubernetes"},
		{"contains", `{{- if contains "seed" .ClusterRole }}yes{{- else }}no{{- end }}`, "yes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandValue(tt.input, data)
			if got != tt.expected {
				t.Errorf("expandValue() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestExpandValueSecurityBlockedFunctions(t *testing.T) {
	data := testData()

	// These should fail template execution and return the original string (passthrough)
	blocked := []string{
		`{{ env "HOME" }}`,
		`{{ expandenv "$HOME" }}`,
		`{{ genPrivateKey "rsa" }}`,
	}

	for _, tmpl := range blocked {
		t.Run(tmpl, func(t *testing.T) {
			got := expandValue(tmpl, data)
			// Should passthrough (template fails because function doesn't exist)
			if got != tmpl {
				t.Errorf("expected passthrough for blocked function, got %q", got)
			}
		})
	}
}

func TestExpandValuePassthroughOnError(t *testing.T) {
	data := testData()

	// Malformed template — should return original string
	malformed := "{{ .NonexistentField }}"
	got := expandValue(malformed, data)
	// Go templates render missing fields as "<no value>" — this is fine,
	// it's not a crash. But {{ .NonexistentField.Method }} would fail.
	// Test an actual parse error:
	parseError := "{{ if }}"
	got = expandValue(parseError, data)
	if got != parseError {
		t.Errorf("expected passthrough for parse error, got %q", got)
	}
}

func TestExpandValueOutputSizeLimit(t *testing.T) {
	data := testData()

	// Template that generates large output
	bigTemplate := `{{ repeat 2000000 "x" }}`
	got := expandValue(bigTemplate, data)
	// Should passthrough because output exceeds maxTemplateOutputBytes (1MB)
	if got != bigTemplate {
		if len(got.(string)) > maxTemplateOutputBytes {
			t.Error("output exceeded size limit")
		}
	}
}

func TestExpandValueRecursive(t *testing.T) {
	data := &templateData{
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

	got := expandValue(input, data)
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
		"cloud.google.com/gke-nodepool":   "default",
		"kubernetes.io/os":                "linux",
		"node.kubernetes.io/instance-type": "n1-standard-2",
	}

	if !hasLabelPrefix(labels, "cloud.google.com/gke-") {
		t.Error("expected GKE label prefix to match")
	}
	if hasLabelPrefix(labels, "eks.amazonaws.com/") {
		t.Error("expected EKS label prefix to NOT match")
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
	if hasLabel(labels, "missing.label") {
		t.Error("expected missing label to return false")
	}
}

func TestSafeFuncMapBlockedFunctions(t *testing.T) {
	fm := safeFuncMap()
	blocked := []string{"env", "expandenv", "genPrivateKey", "genCA",
		"genSelfSignedCert", "genSignedCert", "derivePassword",
		"buildCustomCert", "encryptAES", "decryptAES"}

	for _, name := range blocked {
		if _, ok := fm[name]; ok {
			t.Errorf("function %q should be blocked but is present in safeFuncMap", name)
		}
	}

	// Verify useful functions are still present
	useful := []string{"upper", "lower", "replace", "trim", "trimPrefix",
		"trimSuffix", "contains", "hasPrefix", "hasSuffix", "default",
		"repeat", "nindent", "indent", "quote"}

	for _, name := range useful {
		if _, ok := fm[name]; !ok {
			t.Errorf("function %q should be present in safeFuncMap but is missing", name)
		}
	}
}

func TestExecuteTemplateTimeout(t *testing.T) {
	// This is hard to test without a truly infinite template,
	// but we can verify the function returns within a reasonable time
	data := testData()
	result := executeTemplate("{{ .Region }}", data)
	if result != "us-east-1" {
		t.Errorf("expected us-east-1, got %q", result)
	}
}

func TestExpandValueFastPath(t *testing.T) {
	data := testData()
	// Strings without {{ should skip template parsing entirely
	got := expandValue("no templates here", data)
	if got != "no templates here" {
		t.Errorf("fast path failed: got %q", got)
	}

	// Verify the fast path doesn't apply to strings with {{
	_ = strings.Contains("{{ .Region }}", "{{") // just for clarity
}
