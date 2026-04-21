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
// it afterwards.
func InjectGRMIgnoreAnnotations(manifest []byte) []byte {
	return injectAnnotations(manifest, map[string]string{
		"resources.gardener.cloud/ignore": "true",
	})
}

// InjectGRMJobAnnotations prepares a hook Job for inclusion in a persistent
// ManagedResource:
//   - ignore: GRM creates the Job once and never re-applies it
//   - skip-health-check: completed/failed Jobs don't block MR health
//   - strips ttlSecondsAfterFinished: prevents Kubernetes from deleting the
//     completed Job, which would cause GRM to recreate it on the next
//     reconcile (ignore allows creation when resource is NotFound)
func InjectGRMJobAnnotations(manifest []byte) []byte {
	var obj map[string]interface{}
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		return manifest
	}

	// Strip ttlSecondsAfterFinished from spec — completed Jobs must persist
	// so GRM doesn't recreate them every reconcile cycle.
	if spec, ok := obj["spec"].(map[string]interface{}); ok {
		delete(spec, "ttlSecondsAfterFinished")
	}

	// Add GRM annotations
	metadata, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		metadata = map[string]interface{}{}
		obj["metadata"] = metadata
	}
	annotations, ok := metadata["annotations"].(map[string]interface{})
	if !ok {
		annotations = map[string]interface{}{}
	}
	annotations["resources.gardener.cloud/ignore"] = "true"
	annotations["resources.gardener.cloud/skip-health-check"] = "true"
	metadata["annotations"] = annotations

	out, err := yaml.Marshal(obj)
	if err != nil {
		return manifest
	}
	return out
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
