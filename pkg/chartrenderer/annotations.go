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
// For Job resources with a before-hook-creation delete policy, adds
// resources.gardener.cloud/delete-on-invalid-update: "true" so the GRM
// recreates Jobs when their immutable spec changes between chart versions.
// Jobs without before-hook-creation (one-time Jobs) do NOT get this
// annotation — they should run once and stay completed.
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

	// Read hook-delete-policy BEFORE removing it — needed to decide whether
	// to add delete-on-invalid-update for Job resources.
	deletePolicy, _ := annotations["helm.sh/hook-delete-policy"].(string)
	hasBHC := strings.Contains(deletePolicy, "before-hook-creation")

	// Remove Helm hook annotations
	for _, key := range helmHookAnnotations {
		delete(annotations, key)
	}

	// For Job resources with before-hook-creation policy, add
	// delete-on-invalid-update so the GRM recreates the Job when its
	// immutable spec changes between chart versions.
	//
	// Jobs WITHOUT before-hook-creation (e.g., one-time connector creation
	// Jobs with only hook-succeeded) do NOT get this annotation. They should
	// run once and stay completed — not be recreated every GRM sync cycle.
	kind, _ := obj["kind"].(string)
	if kind == "Job" && hasBHC {
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
