package gcp

import (
	"testing"

	cloudresourcemanager "google.golang.org/api/cloudresourcemanager/v1"
)

func TestAuthMode(t *testing.T) {
	tests := []struct {
		name     string
		creds    Credentials
		expected string
	}{
		{
			name: "static credentials",
			creds: Credentials{
				ServiceAccountJSON: []byte(`{"project_id":"my-project"}`),
			},
			expected: "static",
		},
		{
			name: "workload identity",
			creds: Credentials{
				CredentialsConfig: []byte(`{"audience":"//iam.googleapis.com/..."}`),
				Token:             "eyJhbGciOiJSUzI1NiIs...",
				ProjectID:         "my-project",
			},
			expected: "workload-identity",
		},
		{
			name: "static takes precedence over workload identity",
			creds: Credentials{
				ServiceAccountJSON: []byte(`{"project_id":"my-project"}`),
				CredentialsConfig:  []byte(`{"audience":"..."}`),
				Token:              "token",
			},
			expected: "static",
		},
		{
			name:     "empty credentials",
			creds:    Credentials{},
			expected: "unknown",
		},
		{
			name: "config without token",
			creds: Credentials{
				CredentialsConfig: []byte(`{"audience":"..."}`),
			},
			expected: "unknown",
		},
		{
			name: "token without config",
			creds: Credentials{
				Token: "token",
			},
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.creds.AuthMode()
			if got != tt.expected {
				t.Errorf("AuthMode() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestHasBinding(t *testing.T) {
	policy := &cloudresourcemanager.Policy{
		Bindings: []*cloudresourcemanager.Binding{
			{Role: "roles/logging.logWriter", Members: []string{"serviceAccount:sa@proj.iam.gserviceaccount.com"}},
		},
	}

	if !hasBinding(policy, "roles/logging.logWriter", "serviceAccount:sa@proj.iam.gserviceaccount.com") {
		t.Error("expected binding to exist")
	}
	if hasBinding(policy, "roles/logging.logWriter", "serviceAccount:other@proj.iam.gserviceaccount.com") {
		t.Error("expected binding to not exist")
	}
	if hasBinding(policy, "roles/other.role", "serviceAccount:sa@proj.iam.gserviceaccount.com") {
		t.Error("expected binding to not exist for different role")
	}
}

func TestAddBinding(t *testing.T) {
	policy := &cloudresourcemanager.Policy{}

	// Add to empty policy
	addBinding(policy, "roles/logging.logWriter", "serviceAccount:sa@proj.iam.gserviceaccount.com")
	if !hasBinding(policy, "roles/logging.logWriter", "serviceAccount:sa@proj.iam.gserviceaccount.com") {
		t.Error("expected binding after add")
	}

	// Add second member to same role
	addBinding(policy, "roles/logging.logWriter", "serviceAccount:sa2@proj.iam.gserviceaccount.com")
	if len(policy.Bindings) != 1 {
		t.Errorf("expected 1 binding, got %d", len(policy.Bindings))
	}
	if len(policy.Bindings[0].Members) != 2 {
		t.Errorf("expected 2 members, got %d", len(policy.Bindings[0].Members))
	}

	// Add different role
	addBinding(policy, "roles/monitoring.metricWriter", "serviceAccount:sa@proj.iam.gserviceaccount.com")
	if len(policy.Bindings) != 2 {
		t.Errorf("expected 2 bindings, got %d", len(policy.Bindings))
	}
}

func TestRemoveBinding(t *testing.T) {
	policy := &cloudresourcemanager.Policy{
		Bindings: []*cloudresourcemanager.Binding{
			{Role: "roles/logging.logWriter", Members: []string{
				"serviceAccount:sa1@proj.iam.gserviceaccount.com",
				"serviceAccount:sa2@proj.iam.gserviceaccount.com",
			}},
		},
	}

	// Remove one member, binding should remain
	removeBinding(policy, "roles/logging.logWriter", "serviceAccount:sa1@proj.iam.gserviceaccount.com")
	if len(policy.Bindings) != 1 {
		t.Errorf("expected 1 binding, got %d", len(policy.Bindings))
	}
	if hasBinding(policy, "roles/logging.logWriter", "serviceAccount:sa1@proj.iam.gserviceaccount.com") {
		t.Error("expected sa1 to be removed")
	}
	if !hasBinding(policy, "roles/logging.logWriter", "serviceAccount:sa2@proj.iam.gserviceaccount.com") {
		t.Error("expected sa2 to remain")
	}

	// Remove last member, binding should be removed entirely
	removeBinding(policy, "roles/logging.logWriter", "serviceAccount:sa2@proj.iam.gserviceaccount.com")
	if len(policy.Bindings) != 0 {
		t.Errorf("expected 0 bindings, got %d", len(policy.Bindings))
	}
}
