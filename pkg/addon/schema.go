package addon

import (
	"encoding/json"
)

// jsonSchema is an internal type used to build a JSON Schema document programmatically.
type jsonSchema struct {
	Schema               string                `json:"$schema,omitempty"`
	Title                string                `json:"title,omitempty"`
	Description          string                `json:"description,omitempty"`
	Type                 string                `json:"type,omitempty"`
	Properties           map[string]*jsonSchema `json:"properties,omitempty"`
	Items                *jsonSchema            `json:"items,omitempty"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties *bool                 `json:"additionalProperties,omitempty"`
	Enum                 []string              `json:"enum,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// GenerateJSONSchema produces a JSON Schema (draft-07) for the AddonManifest format.
// The schema is built programmatically from the Go type definitions.
func GenerateJSONSchema() ([]byte, error) {
	schema := &jsonSchema{
		Schema:      "http://json-schema.org/draft-07/schema#",
		Title:       "AddonManifest",
		Description: "Declares which Helm charts the extension deploys to shoots. Compiled into the binary at build time via go:embed.",
		Type:        "object",
		Required:    []string{"apiVersion", "kind", "defaultNamespace", "addons"},
		Properties: map[string]*jsonSchema{
			"apiVersion": {
				Type:        "string",
				Description: "API version of the manifest format, e.g. addons.gardener.cloud/v1alpha1.",
			},
			"kind": {
				Type:        "string",
				Description: "Must be AddonManifest.",
				Enum:        []string{"AddonManifest"},
			},
			"defaultNamespace": {
				Type:        "string",
				Description: "Default namespace used for addons that do not specify their own namespace.",
			},
			"addons": {
				Type:        "array",
				Description: "List of addon definitions to deploy.",
				Items:       addonSchema(),
			},
		},
	}

	// Add globalGCP schema at the top level alongside globalAWS
	schema.Properties["globalGCP"] = globalGCPSchema()

	return json.MarshalIndent(schema, "", "  ")
}

func addonSchema() *jsonSchema {
	return &jsonSchema{
		Type:        "object",
		Description: "A single addon definition.",
		Required:    []string{"name", "chart", "enabled"},
		Properties: map[string]*jsonSchema{
			"name": {
				Type:        "string",
				Description: "Unique name identifying this addon.",
			},
			"chart": chartSourceSchema(),
			"valuesPath": {
				Type:        "string",
				Description: "Path to a values file relative to the addons directory.",
			},
			"namespace": {
				Type:        "string",
				Description: "Target namespace for this addon. Overrides defaultNamespace if set.",
			},
			"managedResourceName": {
				Type:        "string",
				Description: "Name of the Gardener ManagedResource. Defaults to addon-<name>.",
			},
			"enabled": {
				Type:        "boolean",
				Description: "Whether this addon is enabled by default.",
			},
			"shootValues": {
				Type:                 "object",
				Description:          "Additional Helm values injected from shoot metadata at reconciliation time.",
				AdditionalProperties: boolPtr(true),
			},
			"image":  imageOverrideSchema(),
			"aws":    awsAddonConfigSchema(),
		},
	}
}

func chartSourceSchema() *jsonSchema {
	return &jsonSchema{
		Type:        "object",
		Description: "Source location of the Helm chart. Exactly one source type must be specified.",
		Properties: map[string]*jsonSchema{
			"path": {
				Type:        "string",
				Description: "Local path relative to the addons directory (used after make prepare).",
			},
			"oci": {
				Type:        "string",
				Description: "OCI chart reference, e.g. oci://registry/repo/chart.",
			},
			"repo": {
				Type:        "string",
				Description: "Helm repository URL, e.g. https://example.github.io/charts.",
			},
			"repoChart": {
				Type:        "string",
				Description: "Chart name within the Helm repository.",
			},
			"git": {
				Type:        "string",
				Description: "Git repository URL containing the chart.",
			},
			"gitPath": {
				Type:        "string",
				Description: "Path within the git repository to the chart directory.",
			},
			"gitRef": {
				Type:        "string",
				Description: "Git ref (branch, tag, or commit) to check out.",
			},
			"version": {
				Type:        "string",
				Description: "Chart version, used as OCI tag or Helm repo version constraint.",
			},
		},
	}
}

func imageOverrideSchema() *jsonSchema {
	return &jsonSchema{
		Type:        "object",
		Description: "Image override configuration for air-gapped or private registry environments.",
		Required:    []string{"valuesKey"},
		Properties: map[string]*jsonSchema{
			"valuesKey": {
				Type:        "string",
				Description: "Helm values key path for the image field, e.g. image.repository.",
			},
			"defaultRepository": {
				Type:        "string",
				Description: "Default container image repository if not overridden.",
			},
			"defaultTag": {
				Type:        "string",
				Description: "Default container image tag if not overridden.",
			},
		},
	}
}

func globalGCPSchema() *jsonSchema {
	return &jsonSchema{
		Type:        "object",
		Description: "GCP infrastructure applied to every shoot where the extension is enabled and the provider is GCP.",
		Properties: map[string]*jsonSchema{
			"iamRoles": {
				Type:        "array",
				Description: "GCP IAM roles bound to the shoot's node service account at the project level. Examples: roles/logging.logWriter, roles/monitoring.metricWriter.",
				Items: &jsonSchema{
					Type: "string",
				},
			},
		},
	}
}

func awsAddonConfigSchema() *jsonSchema {
	return &jsonSchema{
		Type:        "object",
		Description: "AWS-specific configuration for the addon.",
		Properties: map[string]*jsonSchema{
			"iamPolicies": {
				Type:        "array",
				Description: "List of IAM policy ARNs to attach to the addon's service account role.",
				Items: &jsonSchema{
					Type: "string",
				},
			},
			"vpcEndpoint": {
				Type:        "object",
				Description: "VPC endpoint configuration for the addon.",
				Required:    []string{"service"},
				Properties: map[string]*jsonSchema{
					"service": {
						Type:        "string",
						Description: "AWS service name suffix, e.g. 'logs' resolves to com.amazonaws.<region>.logs.",
					},
				},
			},
		},
	}
}
