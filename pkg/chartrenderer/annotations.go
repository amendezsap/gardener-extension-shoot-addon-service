// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package chartrenderer

import (
	"strings"

	"sigs.k8s.io/yaml"
)

// helmHookAnnotations are the annotation keys to strip from hook resources
// when including them as regular MR objects.
var helmHookAnnotations = []string{
	"helm.sh/hook",
	"helm.sh/hook-weight",
	"helm.sh/hook-delete-policy",
}

// StripHookAnnotations removes helm.sh/hook* annotations from a YAML
// manifest string. Handles multi-document YAML (--- separated).
//
// This function processes non-Job hook resources (Secrets, SAs, RBAC) that
// go into the MR. Hook Jobs are routed to direct application by the
// hook-aware renderer and do not pass through this function.
func StripHookAnnotations(manifest string) string {
	docs := strings.Split(manifest, "\n---\n")
	var result []string

	for _, doc := range docs {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		processed := processDocument(doc)
		if processed != "" {
			result = append(result, processed)
		}
	}

	return strings.Join(result, "\n---\n")
}

func processDocument(doc string) string {
	// Parse the YAML to manipulate annotations
	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		return doc // can't parse, return as-is
	}

	metadata, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		return doc
	}

	annotations, ok := metadata["annotations"].(map[string]interface{})
	if !ok {
		return doc // no annotations
	}

	// Remove Helm hook annotations
	for _, key := range helmHookAnnotations {
		delete(annotations, key)
	}

	// Note: No delete-on-invalid-update is added for Jobs. ALL hook Jobs
	// are routed to direct application (not MR) by the hook-aware renderer.
	// This function only processes non-Job hook resources (Secrets, SAs, RBAC)
	// that go into the MR.

	// Clean up empty annotations
	if len(annotations) == 0 {
		delete(metadata, "annotations")
	} else {
		metadata["annotations"] = annotations
	}

	// Re-serialize
	out, err := yaml.Marshal(obj)
	if err != nil {
		return doc // marshal failed, return original
	}

	return string(out)
}

// InjectGRMIgnoreAnnotations adds resources.gardener.cloud/ignore to a YAML
// manifest. The GRM creates the resource on first reconcile but never updates
// it afterwards. Used for hook Secrets that get populated by hook Jobs — the
// GRM must not overwrite the populated data with the empty chart template.
func InjectGRMIgnoreAnnotations(manifest []byte) []byte {
	return injectAnnotations(manifest, map[string]string{
		"resources.gardener.cloud/ignore": "true",
	})
}

// InjectGRMJobAnnotations adds annotations for hook Jobs in the MR:
//   - ignore: GRM creates the Job once and never re-applies it (Job spec is
//     immutable, and admission mutations cause perpetual diffs)
//   - delete-on-invalid-update: on chart upgrade with a new Job spec, the GRM
//     deletes the old Job and creates the new one instead of failing on
//     immutable field validation
//   - skip-health-check: completed/failed Jobs shouldn't block MR health
func InjectGRMJobAnnotations(manifest []byte) []byte {
	return injectAnnotations(manifest, map[string]string{
		"resources.gardener.cloud/ignore":                 "true",
		"resources.gardener.cloud/delete-on-invalid-update": "true",
		"resources.gardener.cloud/skip-health-check":      "true",
	})
}

func injectAnnotations(manifest []byte, toAdd map[string]string) []byte {
	var obj map[string]interface{}
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		return manifest
	}

	metadata, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		metadata = map[string]interface{}{}
		obj["metadata"] = metadata
	}

	annotations, ok := metadata["annotations"].(map[string]interface{})
	if !ok {
		annotations = map[string]interface{}{}
	}

	for k, v := range toAdd {
		annotations[k] = v
	}
	metadata["annotations"] = annotations

	out, err := yaml.Marshal(obj)
	if err != nil {
		return manifest
	}

	return out
}
