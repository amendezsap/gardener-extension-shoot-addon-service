package addon

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

// AddonManifest declares which Helm charts the extension deploys to shoots.
// Compiled into the binary at build time via go:embed.
type AddonManifest struct {
	APIVersion       string           `json:"apiVersion" yaml:"apiVersion"`
	Kind             string           `json:"kind" yaml:"kind"`
	DefaultNamespace string           `json:"defaultNamespace" yaml:"defaultNamespace"`
	Addons           []Addon          `json:"addons" yaml:"addons"`
	RegistrySecrets  []RegistrySecret `json:"registrySecrets,omitempty" yaml:"registrySecrets,omitempty"`

	// GlobalAWS defines AWS infrastructure that is always provisioned when the
	// extension is active on a shoot, regardless of which addons are enabled.
	// Use this for node-level policies (SSM, CloudWatch) that aren't tied to
	// any specific addon.
	GlobalAWS *GlobalAWSConfig `json:"globalAWS,omitempty" yaml:"globalAWS,omitempty"`

	// GlobalGCP defines GCP infrastructure that is always provisioned when the
	// extension is active on a GCP shoot, regardless of which addons are enabled.
	// Use this for project-level IAM role bindings that aren't tied to any
	// specific addon.
	GlobalGCP *GlobalGCPConfig `json:"globalGCP,omitempty" yaml:"globalGCP,omitempty"`
}

// GlobalAWSConfig defines AWS infrastructure applied to every shoot where
// the extension is enabled. These are VPC/node-level concerns, not addon-specific.
type GlobalAWSConfig struct {
	// IAMPolicies are attached to the shoot's node role. These apply regardless
	// of which addons are enabled. Examples: CloudWatchAgentServerPolicy,
	// AmazonSSMManagedInstanceCore.
	IAMPolicies []string `json:"iamPolicies,omitempty" yaml:"iamPolicies,omitempty"`

	// VPCEndpoints are Interface VPC endpoints created in the shoot's VPC.
	// These are VPC-level infrastructure shared by all addons/workloads.
	// The Gardener node security group is attached automatically.
	// Enabled/disabled via Helm value defaults.aws.vpcEndpoint.enabled
	// and per-shoot providerConfig override.
	VPCEndpoints []VPCEndpointSpec `json:"vpcEndpoints,omitempty" yaml:"vpcEndpoints,omitempty"`
}

// GlobalGCPConfig defines GCP infrastructure applied to every shoot where
// the extension is enabled and the provider is GCP. These are project-level
// IAM concerns, not addon-specific.
type GlobalGCPConfig struct {
	// IAMRoles are GCP IAM roles bound to the shoot's node service account
	// at the project level. These apply regardless of which addons are enabled.
	// Examples: roles/logging.logWriter, roles/monitoring.metricWriter.
	IAMRoles []string `json:"iamRoles,omitempty" yaml:"iamRoles,omitempty"`
}

// AddonTarget specifies where an addon should be deployed.
//   - "shoot"  — deploy to each shoot via shoot-class ManagedResource (default)
//   - "seed"   — deploy once to the seed/runtime cluster via seed-class ManagedResource
//   - "global" — deploy to both shoots and the seed/runtime cluster
type AddonTarget string

const (
	AddonTargetShoot  AddonTarget = "shoot"
	AddonTargetSeed   AddonTarget = "seed"
	AddonTargetGlobal AddonTarget = "global"
)

type Addon struct {
	Name                string                 `json:"name" yaml:"name"`
	Chart               ChartSource            `json:"chart" yaml:"chart"`
	ValuesPath          string                 `json:"valuesPath,omitempty" yaml:"valuesPath,omitempty"`
	Namespace           string                 `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	ManagedResourceName string                 `json:"managedResourceName,omitempty" yaml:"managedResourceName,omitempty"`
	Enabled             bool                   `json:"enabled" yaml:"enabled"`
	Target              AddonTarget            `json:"target,omitempty" yaml:"target,omitempty"`
	ShootValues         map[string]interface{} `json:"shootValues,omitempty" yaml:"shootValues,omitempty"`
	Image               *ImageOverride         `json:"image,omitempty" yaml:"image,omitempty"`
	ImagePullSecrets    []string               `json:"imagePullSecrets,omitempty" yaml:"imagePullSecrets,omitempty"`
	// KeepObjectsOnRename is reserved for future use. Legacy MR cleanup always
	// uses keepObjects=true to preserve resources during MR name migration.
	// The Helm release name is stable (addon name), so all resource types
	// including DaemonSets are safe to preserve.
	KeepObjectsOnRename bool `json:"keepObjectsOnRename,omitempty" yaml:"keepObjectsOnRename,omitempty"`

	// Hooks controls Helm hook rendering behavior. When nil, hooks are
	// silently dropped (historical Gardener behavior). Set hooks.include: true
	// to render hook-annotated templates.
	Hooks *AddonHookConfig `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// AddonHookConfig controls Helm hook rendering for an addon.
type AddonHookConfig struct {
	// Include enables rendering of Helm hook-annotated templates.
	// Install/upgrade hooks are included in the MR as regular resources.
	// Delete hooks are stored separately and executed during addon removal.
	Include bool `json:"include" yaml:"include"`

	// StripAnnotations removes helm.sh/hook* annotations from included
	// hook resources. Defaults to true when not set.
	StripAnnotations *bool `json:"stripAnnotations,omitempty" yaml:"stripAnnotations,omitempty"`

	// DeleteTimeout is the maximum seconds to wait for pre/post-delete
	// hook Jobs to complete during addon removal. Defaults to 300.
	DeleteTimeout *int `json:"deleteTimeout,omitempty" yaml:"deleteTimeout,omitempty"`

	// DeleteFailurePolicy controls behavior when a pre/post-delete hook
	// fails or times out. "Continue" (default) proceeds with MR deletion.
	// "Abort" returns an error, blocking addon removal until the hook succeeds.
	DeleteFailurePolicy string `json:"deleteFailurePolicy,omitempty" yaml:"deleteFailurePolicy,omitempty"`

	// ExcludeTypes lists hook types to exclude. Defaults to ["test"].
	// Valid values: pre-install, post-install, pre-upgrade, post-upgrade,
	// pre-delete, post-delete, pre-rollback, post-rollback, test.
	ExcludeTypes []string `json:"excludeTypes,omitempty" yaml:"excludeTypes,omitempty"`
}

// ShouldStripAnnotations returns whether hook annotations should be stripped.
func (h *AddonHookConfig) ShouldStripAnnotations() bool {
	if h.StripAnnotations == nil {
		return true // default
	}
	return *h.StripAnnotations
}

// GetDeleteTimeout returns the delete hook timeout in seconds.
func (h *AddonHookConfig) GetDeleteTimeout() int {
	if h.DeleteTimeout == nil {
		return 300 // default
	}
	return *h.DeleteTimeout
}

// GetDeleteFailurePolicy returns the delete hook failure policy.
// Defaults to "Continue".
func (h *AddonHookConfig) GetDeleteFailurePolicy() string {
	if strings.EqualFold(h.DeleteFailurePolicy, "abort") {
		return "Abort"
	}
	return "Continue"
}

// ShouldAbortOnDeleteFailure returns true if delete hooks should block
// addon removal on failure.
func (h *AddonHookConfig) ShouldAbortOnDeleteFailure() bool {
	return h.GetDeleteFailurePolicy() == "Abort"
}

// GetExcludeTypes returns the hook types to exclude.
func (h *AddonHookConfig) GetExcludeTypes() []string {
	if len(h.ExcludeTypes) == 0 {
		return []string{"test"}
	}
	return h.ExcludeTypes
}

// GetTarget returns the addon's deployment target, defaulting to "shoot".
func (a *Addon) GetTarget() AddonTarget {
	if a.Target == "" {
		return AddonTargetShoot
	}
	return a.Target
}

// DeploysToShoot returns true if the addon should be deployed to shoots.
func (a *Addon) DeploysToShoot() bool {
	t := a.GetTarget()
	return t == AddonTargetShoot || t == AddonTargetGlobal
}

// DeploysToSeed returns true if the addon should be deployed to the seed/runtime.
func (a *Addon) DeploysToSeed() bool {
	t := a.GetTarget()
	return t == AddonTargetSeed || t == AddonTargetGlobal
}

type ChartSource struct {
	// Local: path relative to addons/ dir (after make prepare)
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// OCI: oci://registry/repo/chart
	OCI string `json:"oci,omitempty" yaml:"oci,omitempty"`
	// Helm repo: https://example.github.io/charts
	Repo      string `json:"repo,omitempty" yaml:"repo,omitempty"`
	RepoChart string `json:"repoChart,omitempty" yaml:"repoChart,omitempty"`
	// Git: https://github.com/org/repo
	Git     string `json:"git,omitempty" yaml:"git,omitempty"`
	GitPath string `json:"gitPath,omitempty" yaml:"gitPath,omitempty"`
	GitRef  string `json:"gitRef,omitempty" yaml:"gitRef,omitempty"`
	// URL: HTTP/HTTPS URL to a chart tarball (.tgz)
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// TGZ: local path to a chart tarball (.tgz)
	TGZ string `json:"tgz,omitempty" yaml:"tgz,omitempty"`
	// Version: used for OCI tag and Helm repo version
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

type ImageOverride struct {
	ValuesKey         string `json:"valuesKey" yaml:"valuesKey"`
	DefaultRepository string `json:"defaultRepository,omitempty" yaml:"defaultRepository,omitempty"`
	DefaultTag        string `json:"defaultTag,omitempty" yaml:"defaultTag,omitempty"`
}

// VPCEndpointSpec declares a VPC endpoint to create in the shoot's VPC.
type VPCEndpointSpec struct {
	// Service is the short service name (e.g., "logs" → com.amazonaws.<region>.logs).
	Service string `json:"service" yaml:"service"`
}

// RegistrySecret declares a registry credential that should be copied from
// the seed cluster to each shoot. The actual credentials live in a Secret
// on the seed; the manifest only contains pointers (names).
type RegistrySecret struct {
	// Name is the name of the dockerconfigjson Secret created on the shoot.
	Name string `json:"name" yaml:"name"`
	// Server is the registry hostname (for documentation, not used in code).
	Server string `json:"server,omitempty" yaml:"server,omitempty"`
	// SeedSecretRef references the Secret on the seed cluster containing
	// the actual registry credentials.
	SeedSecretRef SeedSecretRef `json:"seedSecretRef" yaml:"seedSecretRef"`
}

// SeedSecretRef points to a Secret on the seed cluster.
type SeedSecretRef struct {
	// Name of the Secret on the seed.
	Name string `json:"name" yaml:"name"`
	// Namespace of the Secret on the seed. If empty, uses the extension's namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// GetNamespace returns the addon-specific namespace if set, otherwise the provided default.
func (a *Addon) GetNamespace(defaultNS string) string {
	if a.Namespace != "" {
		return a.Namespace
	}
	return defaultNS
}

// ManagedResourcePrefix is used for shared infrastructure MRs (namespace,
// registry secrets) following Gardener convention. Addon MRs use bare names.
const ManagedResourcePrefix = "extension-shoot-addon-service-"

// GetManagedResourceName returns the ManagedResource name for this addon.
// Uses the explicit managedResourceName if set, otherwise the addon name.
// Addon MRs use bare names (e.g., "fluent-bit") to keep Helm release names
// stable — DaemonSet label selectors are immutable and must not change.
func (a *Addon) GetManagedResourceName() string {
	if a.ManagedResourceName != "" {
		return a.ManagedResourceName
	}
	return a.Name
}

// GetSeedManagedResourceName returns the seed-class ManagedResource name.
func (a *Addon) GetSeedManagedResourceName() string {
	return "seed-" + a.GetManagedResourceName()
}

// Validate checks that the addon definition is well-formed:
//   - Name must be set
//   - Exactly one chart source type must be specified
//   - If chart.Path is used, the path must exist in the embedded FS
func (a *Addon) Validate(efs embed.FS) error {
	if a.Name == "" {
		return fmt.Errorf("addon name is required")
	}

	sources := 0
	if a.Chart.Path != "" {
		sources++
	}
	if a.Chart.OCI != "" {
		sources++
	}
	if a.Chart.Repo != "" {
		sources++
	}
	if a.Chart.Git != "" {
		sources++
	}
	if a.Chart.URL != "" {
		sources++
	}
	if a.Chart.TGZ != "" {
		sources++
	}
	if sources == 0 {
		return fmt.Errorf("addon %q: at least one chart source (path, oci, repo, git, url, tgz) is required", a.Name)
	}
	if sources > 1 {
		return fmt.Errorf("addon %q: exactly one chart source must be specified, got %d", a.Name, sources)
	}

	// If a local path is specified, verify it exists in the embedded FS.
	if a.Chart.Path != "" {
		chartPath := "addons/" + a.Chart.Path
		if _, err := fs.Stat(efs, chartPath); err != nil {
			return fmt.Errorf("addon %q: chart path %q not found in embedded FS: %w", a.Name, chartPath, err)
		}
	}

	return nil
}

// ValidateRemote checks that the addon definition is well-formed for runtime
// (non-embedded) operation. It does not check filesystem paths. For OCI
// chart sources, version is required.
func (a *Addon) ValidateRemote() error {
	if a.Name == "" {
		return fmt.Errorf("addon name is required")
	}

	sources := 0
	if a.Chart.Path != "" {
		sources++
	}
	if a.Chart.OCI != "" {
		sources++
	}
	if a.Chart.Repo != "" {
		sources++
	}
	if a.Chart.Git != "" {
		sources++
	}
	if a.Chart.URL != "" {
		sources++
	}
	if a.Chart.TGZ != "" {
		sources++
	}
	if sources == 0 {
		return fmt.Errorf("addon %q: at least one chart source is required", a.Name)
	}

	if a.Chart.OCI != "" && a.Chart.Version == "" {
		return fmt.Errorf("addon %q: version is required for OCI chart source", a.Name)
	}

	return nil
}

// ReadManifest reads the manifest.yaml from the embedded FS, unmarshals it,
// and validates every addon entry.
func ReadManifest(efs embed.FS) (*AddonManifest, error) {
	data, err := efs.ReadFile("addons/manifest.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading manifest.yaml: %w", err)
	}

	var m AddonManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest.yaml: %w", err)
	}

	if m.APIVersion == "" {
		return nil, fmt.Errorf("manifest.yaml: apiVersion is required")
	}
	if m.Kind == "" {
		return nil, fmt.Errorf("manifest.yaml: kind is required")
	}

	for i := range m.Addons {
		if err := m.Addons[i].Validate(efs); err != nil {
			return nil, fmt.Errorf("manifest.yaml: addon[%d]: %w", i, err)
		}
	}

	return &m, nil
}

// ReadManifestFromData parses a manifest from raw YAML string data (e.g.,
// from a ConfigMap). Unlike ReadManifest, it does not validate chart paths
// against an embedded FS since charts will be pulled from OCI or other
// remote sources at runtime.
func ReadManifestFromData(data string) (*AddonManifest, error) {
	if data == "" {
		return nil, fmt.Errorf("manifest data is empty")
	}

	var m AddonManifest
	if err := yaml.Unmarshal([]byte(data), &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	if m.APIVersion == "" {
		return nil, fmt.Errorf("manifest: apiVersion is required")
	}
	if m.Kind == "" {
		return nil, fmt.Errorf("manifest: kind is required")
	}

	for i := range m.Addons {
		if err := m.Addons[i].ValidateRemote(); err != nil {
			return nil, fmt.Errorf("manifest: addon[%d]: %w", i, err)
		}
	}

	return &m, nil
}
