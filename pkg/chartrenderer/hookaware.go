// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package chartrenderer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"k8s.io/apimachinery/pkg/version"

	gardenerchartrenderer "github.com/gardener/gardener/pkg/chartrenderer"
)

// HookConfig controls how Helm hooks are handled during chart rendering.
type HookConfig struct {
	// Include enables rendering of Helm hook-annotated templates as regular
	// ManagedResource objects. When false (default), hooks are silently
	// dropped — the historical Gardener chartrenderer behavior.
	Include bool

	// StripAnnotations removes helm.sh/hook* annotations from included
	// hook resources before adding them to the MR. Defaults to true.
	StripAnnotations bool

	// DeleteTimeout is the maximum seconds to wait for pre/post-delete
	// hook Jobs to complete during addon removal. Defaults to 300.
	DeleteTimeout int

	// ExcludeTypes lists hook types to exclude even when Include is true.
	// Defaults to ["test"].
	ExcludeTypes []string
}

// DefaultHookConfig returns a HookConfig with sensible defaults.
func DefaultHookConfig() *HookConfig {
	return &HookConfig{
		Include:          true,
		StripAnnotations: true,
		DeleteTimeout:    300,
		ExcludeTypes:     []string{"test"},
	}
}

// RenderResult holds the output of a hook-aware chart render.
type RenderResult struct {
	// MRData is the ManagedResource secret data containing install/upgrade
	// hooks and regular manifests. Same format as RenderedChart.AsSecretData().
	MRData map[string][]byte

	// PreDeleteHooks contains raw YAML manifests for pre-delete hooks.
	// These are NOT included in the MR — the actuator applies them
	// directly during Delete() before removing the MR.
	PreDeleteHooks [][]byte

	// PostDeleteHooks contains raw YAML manifests for post-delete hooks.
	// Applied by the actuator after MR deletion.
	PostDeleteHooks [][]byte

	// OneTimeJobs contains raw YAML manifests for hook Jobs. The actuator
	// handles these based on render context:
	// - Seed renders: applied directly with completion wait (ordering control)
	// - Shoot renders: added to MRData with GRM annotations (ignore +
	//   delete-on-invalid-update) so the GRM creates once and handles
	//   chart upgrades via delete+recreate on immutable field errors.
	OneTimeJobs [][]byte

	// HookSecrets contains raw YAML manifests for hook Secret resources.
	// These are ALSO included in MRData with resources.gardener.cloud/ignore
	// so the GRM creates them once and never overwrites Job-populated data.
	// The actuator uses this list on seed renders to apply Secrets directly
	// before Jobs (ordering: Secret must exist before Job writes to it).
	HookSecrets [][]byte
}

// HookAwareRenderer renders Helm charts including hook-annotated templates.
// Unlike Gardener's chartrenderer which discards hooks, this renderer
// recombines install/upgrade hooks with regular manifests and separately
// captures delete hooks for lifecycle management.
type HookAwareRenderer struct {
	eng          *engine.Engine
	capabilities *chartutil.Capabilities
}

// NewHookAwareRenderer creates a renderer with proper Helm capabilities
// including HelmVersion (from the compiled Helm library).
func NewHookAwareRenderer(serverVersion *version.Info) *HookAwareRenderer {
	caps := chartutil.DefaultCapabilities.Copy()
	if serverVersion != nil {
		caps.KubeVersion = chartutil.KubeVersion{
			Version: serverVersion.GitVersion,
			Major:   serverVersion.Major,
			Minor:   serverVersion.Minor,
		}
	}

	return &HookAwareRenderer{
		eng:          &engine.Engine{},
		capabilities: caps,
	}
}

// RenderArchive loads a chart from an archive and renders it with hooks.
func (r *HookAwareRenderer) RenderArchive(archive []byte, releaseName, namespace string, values map[string]interface{}, hookCfg *HookConfig) (*RenderResult, error) {
	chart, err := helmloader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("load chart from archive: %w", err)
	}
	return r.render(chart, releaseName, namespace, values, hookCfg)
}

// RenderChart renders an already-loaded chart with hooks.
func (r *HookAwareRenderer) RenderChart(chart *helmchart.Chart, releaseName, namespace string, values map[string]interface{}, hookCfg *HookConfig) (*RenderResult, error) {
	return r.render(chart, releaseName, namespace, values, hookCfg)
}

func (r *HookAwareRenderer) render(chart *helmchart.Chart, releaseName, namespace string, values map[string]interface{}, hookCfg *HookConfig) (*RenderResult, error) {
	if hookCfg == nil {
		hookCfg = DefaultHookConfig()
	}

	parsedValues, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}

	vals, err := chartutil.ReadValues(parsedValues)
	if err != nil {
		return nil, fmt.Errorf("read values: %w", err)
	}

	if err := chartutil.ProcessDependencies(chart, vals); err != nil {
		return nil, fmt.Errorf("process dependencies: %w", err)
	}

	options := chartutil.ReleaseOptions{
		Name:      releaseName,
		Namespace: namespace,
		Revision:  1,
		IsInstall: true,
	}

	valuesToRender, err := chartutil.ToRenderValues(chart, vals, options, r.capabilities)
	if err != nil {
		return nil, fmt.Errorf("build render values: %w", err)
	}

	// Render ALL templates — hooks are rendered by the engine
	files, err := r.eng.Render(chart, valuesToRender)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	// Remove NOTES.txt and partials
	for k := range files {
		if strings.HasSuffix(k, "NOTES.txt") || strings.HasPrefix(path.Base(k), "_") {
			delete(files, k)
		}
	}

	// SortManifests separates hooks from regular manifests
	hooks, manifests, err := releaseutil.SortManifests(files, r.capabilities.APIVersions, releaseutil.InstallOrder)
	if err != nil {
		return nil, fmt.Errorf("sort manifests: %w", err)
	}

	// Build the exclude set
	excludeSet := make(map[string]bool)
	for _, t := range hookCfg.ExcludeTypes {
		excludeSet[t] = true
	}

	// Classify hooks into buckets
	var installHookManifests []releaseutil.Manifest
	var preDeleteHooks [][]byte
	var postDeleteHooks [][]byte
	var oneTimeJobs [][]byte
	var hookSecrets [][]byte

	for _, hook := range hooks {
		hookTypes := classifyHook(hook)

		if hookTypes.excluded(excludeSet) {
			continue
		}

		// Store delete hooks for lifecycle management. Hooks with mixed
		// events (e.g., pre-install + pre-delete) are stored for delete
		// AND included in the MR/OneTimeJobs below.
		if hookTypes.isPreDelete {
			preDeleteHooks = append(preDeleteHooks, []byte(hook.Manifest))
		}
		if hookTypes.isPostDelete {
			postDeleteHooks = append(postDeleteHooks, []byte(hook.Manifest))
		}

		// Check if this hook has any install/upgrade event
		hasInstallEvent := false
		for _, t := range hookTypes.types {
			if t == "pre-install" || t == "post-install" || t == "pre-upgrade" || t == "post-upgrade" {
				hasInstallEvent = true
				break
			}
		}

		// Pure delete hooks (only pre-delete/post-delete, no install events)
		// are NOT included in the MR — they're only for the Delete() path.
		if !hasInstallEvent && (hookTypes.isPreDelete || hookTypes.isPostDelete) {
			continue
		}

		content := hook.Manifest
		if hookCfg.StripAnnotations {
			content = StripHookAnnotations(content)
		}

		// Route one-time Jobs to direct application instead of the MR.
		// A one-time Job is a Job without before-hook-creation policy —
		// it should run once and stay completed, not be recreated by the
		// GRM every reconcile cycle. Jobs WITH before-hook-creation go
		// into the MR with delete-on-invalid-update (set by StripHookAnnotations).
		if isHookJob(hook) {
			oneTimeJobs = append(oneTimeJobs, []byte(content))
			continue
		}

		// Hook Secrets get the GRM ignore annotation so they are created
		// once and never overwritten. Hook Jobs may populate them with
		// real data after creation (e.g., connector registration writes
		// credentials into an initially empty Secret). Also saved in
		// HookSecrets for seed-render direct application (ordering).
		if isHookSecret(content) {
			annotated := string(InjectGRMIgnoreAnnotations([]byte(content)))
			hookSecrets = append(hookSecrets, []byte(content))
			installHookManifests = append(installHookManifests, releaseutil.Manifest{
				Name:    hook.Path,
				Content: annotated,
			})
			continue
		}

		// Other hook resources (SAs, Roles, RoleBindings) go into the
		// MR as regular resources. They're idempotent and need to
		// persist for running workloads.
		installHookManifests = append(installHookManifests, releaseutil.Manifest{
			Name:    hook.Path,
			Content: content,
		})
	}

	// Sort install hooks by name for deterministic output
	sort.SliceStable(installHookManifests, func(i, j int) bool {
		return installHookManifests[i].Name < installHookManifests[j].Name
	})

	// Build MR secret data from regular manifests using Gardener's
	// RenderedChart (handles key sanitization and multi-resource splitting)
	rendered := &gardenerchartrenderer.RenderedChart{
		ChartName: chart.Name(),
		Manifests: manifests,
	}
	mrData := rendered.AsSecretData()

	// Add non-Secret hook manifests (SAs, Roles, RoleBindings) to MR data.
	// These are idempotent and need to persist for running workloads.
	hookKeyCounts := make(map[string]int)
	for _, hm := range installHookManifests {
		content := strings.TrimSpace(hm.Content)
		if len(content) == 0 {
			continue
		}
		baseKey := strings.ReplaceAll(hm.Name, "/", "_")
		hookKeyCounts[baseKey]++
		key := baseKey
		if hookKeyCounts[baseKey] > 1 {
			key = strings.TrimSuffix(baseKey, ".yaml") +
				fmt.Sprintf("_%d", hookKeyCounts[baseKey]) + ".yaml"
		}
		mrData[key] = []byte(content)
	}

	return &RenderResult{
		MRData:          mrData,
		PreDeleteHooks:  preDeleteHooks,
		PostDeleteHooks: postDeleteHooks,
		OneTimeJobs:     oneTimeJobs,
		HookSecrets:     hookSecrets,
	}, nil
}

// isHookSecret returns true if the manifest content is a Secret resource.
// Hook Secrets are routed to one-time application because hook Jobs may
// populate them with real data after creation.
func isHookSecret(content string) bool {
	return strings.Contains(content, "kind: Secret")
}

// isHookJob returns true if the hook is a Job resource. ALL hook Jobs are
// applied directly by the actuator (not via MR) because:
//
//   - Helm hook Jobs run ONCE per install/upgrade event
//   - The GRM reconciles MR resources every 60s, recreating completed Jobs
//   - Hook delete policies (before-hook-creation, hook-succeeded) are about
//     cleanup in the Helm lifecycle, not about run frequency
//
// Direct application with skip-if-exists gives the correct "run once" behavior.
func isHookJob(hook *release.Hook) bool {
	return strings.Contains(hook.Manifest, "kind: Job")
}

// hookClassification holds the parsed hook types for a single hook resource.
type hookClassification struct {
	isPreDelete  bool
	isPostDelete bool
	isTest       bool
	types        []string
}

func (h *hookClassification) excluded(excludeSet map[string]bool) bool {
	for _, t := range h.types {
		if excludeSet[t] {
			return true
		}
	}
	return false
}

func classifyHook(hook *release.Hook) *hookClassification {
	c := &hookClassification{}
	for _, event := range hook.Events {
		eventStr := hookEventToString(event)
		c.types = append(c.types, eventStr)
		switch eventStr {
		case "pre-delete":
			c.isPreDelete = true
		case "post-delete":
			c.isPostDelete = true
		case "test":
			c.isTest = true
		}
	}
	return c
}

func hookEventToString(event release.HookEvent) string {
	switch event {
	case release.HookPreInstall:
		return "pre-install"
	case release.HookPostInstall:
		return "post-install"
	case release.HookPreUpgrade:
		return "pre-upgrade"
	case release.HookPostUpgrade:
		return "post-upgrade"
	case release.HookPreDelete:
		return "pre-delete"
	case release.HookPostDelete:
		return "post-delete"
	case release.HookPreRollback:
		return "pre-rollback"
	case release.HookPostRollback:
		return "post-rollback"
	case release.HookTest:
		return "test"
	default:
		return string(event)
	}
}
