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
// For Job resources, adds the Gardener annotation
// resources.gardener.cloud/delete-on-invalid-update: "true" so the GRM
// deletes and recreates Jobs when their immutable spec changes between
// reconciles. This replaces the Helm hook-delete-policy: before-hook-creation
// behavior.
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

	// For Job resources, add delete-on-invalid-update annotation so the GRM
	// can recreate Jobs with changed spec (Jobs are immutable). This replaces
	// the Helm hook-delete-policy: before-hook-creation behavior.
	kind, _ := obj["kind"].(string)
	if kind == "Job" {
		annotations["resources.gardener.cloud/delete-on-invalid-update"] = "true"
	}

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
