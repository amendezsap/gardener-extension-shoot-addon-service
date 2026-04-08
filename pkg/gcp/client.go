package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	cloudresourcemanager "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/option"
)

// Credentials holds GCP credentials extracted from the Gardener cloudprovider secret.
type Credentials struct {
	ServiceAccountJSON []byte
}

// Client wraps GCP API clients for IAM operations.
type Client struct {
	crm       *cloudresourcemanager.Service
	projectID string
}

// maxRetries is the number of times to retry on etag conflict.
const maxRetries = 5

// NewClient creates a GCP client from Gardener cloudprovider credentials.
func NewClient(creds *Credentials) (*Client, error) {
	if len(creds.ServiceAccountJSON) == 0 {
		return nil, fmt.Errorf("service account JSON is empty")
	}

	// Parse project ID from service account JSON
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(creds.ServiceAccountJSON, &sa); err != nil {
		return nil, fmt.Errorf("parse service account JSON: %w", err)
	}
	if sa.ProjectID == "" {
		return nil, fmt.Errorf("service account JSON missing project_id")
	}

	ctx := context.Background()
	jwtConfig, err := google.JWTConfigFromJSON(creds.ServiceAccountJSON,
		cloudresourcemanager.CloudPlatformScope,
	)
	if err != nil {
		return nil, fmt.Errorf("create JWT config: %w", err)
	}

	httpClient := jwtConfig.Client(ctx)

	crm, err := cloudresourcemanager.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create cloudresourcemanager client: %w", err)
	}

	return &Client{
		crm:       crm,
		projectID: sa.ProjectID,
	}, nil
}

// ProjectID returns the project ID parsed from the service account credentials.
func (c *Client) ProjectID() string {
	return c.projectID
}

// AddIAMPolicyBinding adds a role binding for the given member on the project.
// Idempotent: if the binding already exists, this is a no-op.
// Handles etag-based concurrency by retrying on conflict.
func (c *Client) AddIAMPolicyBinding(ctx context.Context, member string, role string) error {
	for attempt := 0; attempt < maxRetries; attempt++ {
		policy, err := c.crm.Projects.GetIamPolicy(c.projectID,
			&cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get IAM policy for project %s: %w", c.projectID, err)
		}

		// Check if binding already exists
		if hasBinding(policy, role, member) {
			return nil // already bound
		}

		// Add the binding
		addBinding(policy, role, member)

		_, err = c.crm.Projects.SetIamPolicy(c.projectID,
			&cloudresourcemanager.SetIamPolicyRequest{
				Policy: policy,
			}).Context(ctx).Do()
		if err != nil {
			if isConflictError(err) && attempt < maxRetries-1 {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("set IAM policy for project %s: %w", c.projectID, err)
		}
		return nil
	}
	return fmt.Errorf("failed to add IAM binding after %d retries (etag conflict)", maxRetries)
}

// RemoveIAMPolicyBinding removes a role binding for the given member on the project.
// Idempotent: if the binding does not exist, this is a no-op.
// Handles etag-based concurrency by retrying on conflict.
func (c *Client) RemoveIAMPolicyBinding(ctx context.Context, member string, role string) error {
	for attempt := 0; attempt < maxRetries; attempt++ {
		policy, err := c.crm.Projects.GetIamPolicy(c.projectID,
			&cloudresourcemanager.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("get IAM policy for project %s: %w", c.projectID, err)
		}

		// Check if binding exists
		if !hasBinding(policy, role, member) {
			return nil // already removed
		}

		// Remove the binding
		removeBinding(policy, role, member)

		_, err = c.crm.Projects.SetIamPolicy(c.projectID,
			&cloudresourcemanager.SetIamPolicyRequest{
				Policy: policy,
			}).Context(ctx).Do()
		if err != nil {
			if isConflictError(err) && attempt < maxRetries-1 {
				time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
				continue
			}
			return fmt.Errorf("set IAM policy for project %s: %w", c.projectID, err)
		}
		return nil
	}
	return fmt.Errorf("failed to remove IAM binding after %d retries (etag conflict)", maxRetries)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// hasBinding checks if the policy contains a binding for the given role and member.
func hasBinding(policy *cloudresourcemanager.Policy, role, member string) bool {
	for _, b := range policy.Bindings {
		if b.Role == role {
			for _, m := range b.Members {
				if m == member {
					return true
				}
			}
		}
	}
	return false
}

// addBinding adds a member to a role binding, creating the binding if needed.
func addBinding(policy *cloudresourcemanager.Policy, role, member string) {
	for _, b := range policy.Bindings {
		if b.Role == role {
			b.Members = append(b.Members, member)
			return
		}
	}
	// Role binding doesn't exist yet — create it
	policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
		Role:    role,
		Members: []string{member},
	})
}

// removeBinding removes a member from a role binding. If the binding has no
// members left, it is removed from the policy.
func removeBinding(policy *cloudresourcemanager.Policy, role, member string) {
	for i, b := range policy.Bindings {
		if b.Role != role {
			continue
		}
		var remaining []string
		for _, m := range b.Members {
			if m != member {
				remaining = append(remaining, m)
			}
		}
		if len(remaining) == 0 {
			// Remove the entire binding
			policy.Bindings = append(policy.Bindings[:i], policy.Bindings[i+1:]...)
		} else {
			b.Members = remaining
		}
		return
	}
}

// isConflictError checks if an error is an etag conflict (HTTP 409).
func isConflictError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "409")
}
