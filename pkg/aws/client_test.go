package aws

import (
	"testing"
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
				AccessKeyID:    "AKIA...",
				SecretAccessKey: "secret",
			},
			expected: "static",
		},
		{
			name: "static with role and token",
			creds: Credentials{
				AccessKeyID:    "AKIA...",
				SecretAccessKey: "secret",
				RoleARN:        "arn:aws:iam::123456:role/my-role",
				Token:          "session-token",
			},
			expected: "static",
		},
		{
			name: "workload identity",
			creds: Credentials{
				RoleARN:          "arn:aws:iam::123456:role/my-role",
				WebIdentityToken: "eyJhbGciOiJSUzI1NiIs...",
			},
			expected: "workload-identity",
		},
		{
			name: "workload identity with empty access key",
			creds: Credentials{
				AccessKeyID:      "",
				RoleARN:          "arn:aws:iam::123456:role/my-role",
				WebIdentityToken: "token",
			},
			expected: "workload-identity",
		},
		{
			name:     "empty credentials",
			creds:    Credentials{},
			expected: "unknown",
		},
		{
			name: "role without token",
			creds: Credentials{
				RoleARN: "arn:aws:iam::123456:role/my-role",
			},
			expected: "unknown",
		},
		{
			name: "token without role",
			creds: Credentials{
				WebIdentityToken: "token",
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

func TestIdentityTokenRetriever(t *testing.T) {
	token := "my-web-identity-token"
	retriever := IdentityTokenRetriever(token)

	got, err := retriever.GetIdentityToken()
	if err != nil {
		t.Fatalf("GetIdentityToken() error = %v", err)
	}
	if string(got) != token {
		t.Errorf("GetIdentityToken() = %q, want %q", string(got), token)
	}
}

func TestIdentityTokenRetrieverEmpty(t *testing.T) {
	retriever := IdentityTokenRetriever("")

	got, err := retriever.GetIdentityToken()
	if err != nil {
		t.Fatalf("GetIdentityToken() error = %v", err)
	}
	if string(got) != "" {
		t.Errorf("GetIdentityToken() = %q, want empty", string(got))
	}
}
