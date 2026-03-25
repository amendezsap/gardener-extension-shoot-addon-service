package grm

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var grmConfigMapRegex = regexp.MustCompile(`^gardener-resource-manager-[a-f0-9]{8}$`)

const WebhookName = "grm-namespace-provisioner"

type Mutator struct{}

func NewMutator() *Mutator {
	return &Mutator{}
}

// Mutate injects required namespaces into the GRM ConfigMap's
// targetClientConnection.namespaces list. This ensures the GRM's
// controller-runtime cache watches the namespaces our addons deploy to
// (e.g., managed-resources), in addition to the default gardenlet namespaces.
//
// Idempotent — only adds namespaces that are not already in the list.
func (m *Mutator) Mutate(ctx context.Context, newObj, oldObj client.Object) error {
	cm, ok := newObj.(*corev1.ConfigMap)
	if !ok || cm.DeletionTimestamp != nil {
		return nil
	}
	if !grmConfigMapRegex.MatchString(cm.Name) {
		return nil
	}
	configYAML, ok := cm.Data["config.yaml"]
	if !ok || configYAML == "" {
		return nil
	}
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &config); err != nil {
		return nil
	}
	tcc, ok := config["targetClientConnection"]
	if !ok {
		return nil
	}
	tccMap, ok := tcc.(map[string]interface{})
	if !ok {
		return nil
	}

	required := LoadNamespacesFromEnv()
	if len(required) == 0 {
		return nil
	}

	// Get existing namespaces list (may be absent)
	var existing []string
	if nsRaw, ok := tccMap["namespaces"]; ok {
		if nsList, ok := nsRaw.([]interface{}); ok {
			for _, v := range nsList {
				if s, ok := v.(string); ok {
					existing = append(existing, s)
				}
			}
		}
	}

	// Check if all required namespaces are already present
	existingSet := make(map[string]bool, len(existing))
	for _, ns := range existing {
		existingSet[ns] = true
	}
	var added []string
	for _, ns := range required {
		if !existingSet[ns] {
			existing = append(existing, ns)
			added = append(added, ns)
		}
	}

	if len(added) == 0 {
		return nil
	}

	// Convert to []interface{} for YAML marshaling
	nsList := make([]interface{}, len(existing))
	for i, ns := range existing {
		nsList[i] = ns
	}
	tccMap["namespaces"] = nsList
	config["targetClientConnection"] = tccMap

	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config.yaml: %w", err)
	}
	cm.Data["config.yaml"] = string(newYAML)

	log.FromContext(ctx).Info("Injected namespaces into GRM ConfigMap",
		"name", cm.Name, "namespace", cm.Namespace, "added", added, "total", existing)
	return nil
}

func LoadNamespacesFromEnv() []string {
	raw := os.Getenv("NAMESPACES")
	if raw == "" {
		return []string{"observability"}
	}
	var ns []string
	for _, n := range strings.Split(raw, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			ns = append(ns, n)
		}
	}
	return ns
}
