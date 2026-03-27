package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AddonServiceConfig is the providerConfig for the shoot-addon-service extension.
// It allows per-addon overrides and AWS-specific configuration.
type AddonServiceConfig struct {
	Addons map[string]AddonOverride `json:"addons,omitempty"`
	AWS    *AWSOverride             `json:"aws,omitempty"`
}

// AddonOverride allows enabling/disabling individual addons and overriding
// their values on a per-shoot basis.
type AddonOverride struct {
	Enabled *bool `json:"enabled,omitempty"`
	// ValuesOverride is a YAML string of values to merge into (or replace)
	// the addon's values for this shoot only. Useful for debugging or
	// per-shoot customization.
	ValuesOverride string `json:"valuesOverride,omitempty"`
	// ValuesMode controls how ValuesOverride is applied:
	//   "merge" (default) — deep-merge with existing values, only specified keys change
	//   "override" — replace all values entirely with ValuesOverride
	ValuesMode string `json:"valuesMode,omitempty"`
}

// IsOverrideMode returns true if ValuesMode is "override" (full replace).
// Default is merge (additive).
func (o *AddonOverride) IsOverrideMode() bool {
	return strings.EqualFold(o.ValuesMode, "override")
}

// AWSOverride holds AWS-specific configuration overrides.
type AWSOverride struct {
	VPCEndpoint *VPCEndpointOverride `json:"vpcEndpoint,omitempty"`
}

// VPCEndpointOverride allows enabling/disabling VPC endpoint management.
type VPCEndpointOverride struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// ProviderStatus tracks state persisted on the Extension resource.
type ProviderStatus struct {
	Addons map[string]*AddonStatus `json:"addons,omitempty"`
	// GlobalIAMPolicies tracks which global IAM policies were attached
	// on the last reconcile. Used to detect removals and detach stale policies.
	GlobalIAMPolicies []string `json:"globalIAMPolicies,omitempty"`
	// VPCEndpoint tracks the global VPC endpoint state.
	VPCEndpoint *VPCEndpointStatus `json:"vpcEndpoint,omitempty"`
}

// AddonStatus holds the state for a single addon.
type AddonStatus struct {
}

// VPCEndpointStatus tracks VPC endpoint state for an addon.
type VPCEndpointStatus struct {
	Enabled             bool   `json:"enabled"`
	EndpointID          string `json:"endpointID,omitempty"`
	VPCID               string `json:"vpcID,omitempty"`
	NodeSecurityGroupID string `json:"nodeSecurityGroupID,omitempty"`
	CreatedByUs         bool   `json:"createdByUs"`
}

// ResolveConfig parses the Extension CR's providerConfig into an AddonServiceConfig.
// Returns a zero-value config if providerConfig is nil or empty.
func ResolveConfig(ex *extensionsv1alpha1.Extension) (*AddonServiceConfig, error) {
	cfg := &AddonServiceConfig{}
	if ex == nil || ex.Spec.ProviderConfig == nil {
		return cfg, nil
	}

	raw := ex.Spec.ProviderConfig.Raw
	if len(raw) == 0 {
		return cfg, nil
	}

	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal addon service config: %w", err)
	}
	return cfg, nil
}

// GetPreviousStatus parses the Extension CR's status.providerStatus into a ProviderStatus.
// Returns a zero-value status if providerStatus is nil or empty.
func GetPreviousStatus(ex *extensionsv1alpha1.Extension) (*ProviderStatus, error) {
	status := &ProviderStatus{}
	if ex == nil || ex.Status.ProviderStatus == nil {
		return status, nil
	}

	raw := ex.Status.ProviderStatus.Raw
	if len(raw) == 0 {
		return status, nil
	}

	if err := json.Unmarshal(raw, status); err != nil {
		return nil, fmt.Errorf("unmarshal provider status: %w", err)
	}
	return status, nil
}

// MarshalProviderStatus serializes a ProviderStatus for storing in Extension status.
func MarshalProviderStatus(status *ProviderStatus) (*runtime.RawExtension, error) {
	raw, err := json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("marshal provider status: %w", err)
	}
	return &runtime.RawExtension{Raw: raw}, nil
}

// IsAddonEnabled checks whether a named addon should be enabled.
// It first checks the config for an explicit override; if none exists, it falls
// back to the manifestEnabled value (i.e., what the shoot manifest says).
func (c *AddonServiceConfig) IsAddonEnabled(addonName string, manifestEnabled bool) bool {
	if c.Addons != nil {
		if override, ok := c.Addons[addonName]; ok && override.Enabled != nil {
			return *override.Enabled
		}
	}
	return manifestEnabled
}

// IsVPCEndpointEnabled returns whether VPC endpoint management is enabled.
// Priority: explicit config override > DEFAULT_VPC_ENDPOINT_ENABLED env var > false.
func (c *AddonServiceConfig) IsVPCEndpointEnabled() bool {
	if c.AWS != nil && c.AWS.VPCEndpoint != nil && c.AWS.VPCEndpoint.Enabled != nil {
		return *c.AWS.VPCEndpoint.Enabled
	}

	envVal := os.Getenv("DEFAULT_VPC_ENDPOINT_ENABLED")
	if envVal != "" {
		envVal = strings.TrimSpace(strings.ToLower(envVal))
		b, err := strconv.ParseBool(envVal)
		if err == nil {
			return b
		}
	}

	return false
}
