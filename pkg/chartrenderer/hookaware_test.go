// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package chartrenderer

import (
	"testing"

	"helm.sh/helm/v3/pkg/release"
)

func TestStripHookAnnotations(t *testing.T) {
	input := `apiVersion: batch/v1
kind: Job
metadata:
  name: create-connector
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "-1"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
    app.kubernetes.io/managed-by: Helm
spec:
  template:
    spec:
      containers:
        - name: connector
          image: example/connector:latest`

	result := StripHookAnnotations(input)

	// Should NOT contain hook annotations
	if contains(result, "helm.sh/hook") {
		t.Error("helm.sh/hook annotation not stripped")
	}
	if contains(result, "helm.sh/hook-weight") {
		t.Error("helm.sh/hook-weight annotation not stripped")
	}
	if contains(result, "helm.sh/hook-delete-policy") {
		t.Error("helm.sh/hook-delete-policy annotation not stripped")
	}

	// Should still contain non-hook annotations
	if !contains(result, "app.kubernetes.io/managed-by") {
		t.Error("non-hook annotation was incorrectly stripped")
	}

	// Should contain the Job resource
	if !contains(result, "kind: Job") {
		t.Error("Job kind missing from output")
	}

	// Should have delete-on-invalid-update because this Job has before-hook-creation
	if !contains(result, "resources.gardener.cloud/delete-on-invalid-update") {
		t.Error("Job with before-hook-creation should have delete-on-invalid-update annotation")
	}
}

func TestStripHookAnnotationsOneTimeJob(t *testing.T) {
	// Job with only hook-succeeded (no before-hook-creation) — one-time Job
	input := `apiVersion: batch/v1
kind: Job
metadata:
  name: create-connector
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-delete-policy": hook-succeeded
spec:
  template:
    spec:
      containers:
        - name: connector
          image: example/connector:latest`

	result := StripHookAnnotations(input)

	// Should NOT have delete-on-invalid-update — one-time Jobs should run once
	if contains(result, "delete-on-invalid-update") {
		t.Error("one-time Job (no before-hook-creation) should NOT have delete-on-invalid-update")
	}
}

func TestStripHookAnnotationsJobNoDeletePolicy(t *testing.T) {
	// Job with no hook-delete-policy at all — treat as one-time
	input := `apiVersion: batch/v1
kind: Job
metadata:
  name: setup-job
  annotations:
    "helm.sh/hook": pre-install
spec:
  template:
    spec:
      containers:
        - name: setup
          image: example/setup:latest`

	result := StripHookAnnotations(input)

	if contains(result, "delete-on-invalid-update") {
		t.Error("Job without any delete policy should NOT have delete-on-invalid-update")
	}
}

func TestStripHookAnnotationsEmptyAnnotations(t *testing.T) {
	input := `apiVersion: v1
kind: Secret
metadata:
  name: my-secret
  annotations:
    "helm.sh/hook": pre-install
spec:
  data: {}`

	result := StripHookAnnotations(input)

	// annotations block should be removed since it's now empty
	if contains(result, "annotations:") && !contains(result, "resources.gardener.cloud") {
		t.Error("empty annotations block should be removed")
	}
}

func TestStripHookAnnotationsNoHooks(t *testing.T) {
	input := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    app.kubernetes.io/version: "1.0"
spec:
  replicas: 1`

	result := StripHookAnnotations(input)

	// Should be unchanged (no hook annotations to strip)
	if !contains(result, "app.kubernetes.io/version") {
		t.Error("non-hook annotation was incorrectly stripped")
	}
	if !contains(result, "replicas") {
		t.Error("spec was incorrectly modified")
	}
}

func TestStripHookAnnotationsMultiDoc(t *testing.T) {
	input := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-sa
  annotations:
    "helm.sh/hook": pre-install
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: my-role
  annotations:
    "helm.sh/hook": pre-install
    "helm.sh/hook-weight": "-1"`

	result := StripHookAnnotations(input)

	if contains(result, "helm.sh/hook") {
		t.Error("hook annotations not stripped from multi-doc YAML")
	}
	if !contains(result, "kind: ServiceAccount") {
		t.Error("ServiceAccount missing from output")
	}
	if !contains(result, "kind: Role") {
		t.Error("Role missing from output")
	}
}

func TestHookEventToString(t *testing.T) {
	tests := []struct {
		event    string
		expected string
	}{
		{"pre-install", "pre-install"},
		{"post-install", "post-install"},
		{"pre-delete", "pre-delete"},
		{"post-delete", "post-delete"},
		{"pre-upgrade", "pre-upgrade"},
		{"post-upgrade", "post-upgrade"},
		{"test", "test"},
	}

	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			// hookEventToString maps release.HookEvent constants
			// This test validates the string mapping is correct
			got := hookEventToString(hookEventFromString(tt.event))
			if got != tt.expected {
				t.Errorf("hookEventToString(%q) = %q, want %q", tt.event, got, tt.expected)
			}
		})
	}
}

func TestDefaultHookConfig(t *testing.T) {
	cfg := DefaultHookConfig()

	if !cfg.Include {
		t.Error("default Include should be true")
	}
	if !cfg.StripAnnotations {
		t.Error("default StripAnnotations should be true")
	}
	if cfg.DeleteTimeout != 300 {
		t.Errorf("default DeleteTimeout = %d, want 300", cfg.DeleteTimeout)
	}
	if len(cfg.ExcludeTypes) != 1 || cfg.ExcludeTypes[0] != "test" {
		t.Errorf("default ExcludeTypes = %v, want [test]", cfg.ExcludeTypes)
	}
}

func hookEventFromString(s string) release.HookEvent {
	return release.HookEvent(s)
}

// helper
func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) >= len(substr) && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
