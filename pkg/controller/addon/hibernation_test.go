package addon

import (
	"strings"
	"testing"
)

func TestSkipHealthCheckForWorkloads(t *testing.T) {
	secretData := map[string][]byte{
		"deploy.yaml": []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: sample-deploy\n  namespace: managed-resources\n  annotations:\n    existing: keep\nspec:\n  replicas: 1\n"),
		"ds.yaml":     []byte("apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: sample-ds\n"),
		"secret.yaml": []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: sample-secret\n"),
	}

	out, err := skipHealthCheckForWorkloads(secretData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(string(out["deploy.yaml"]), "resources.gardener.cloud/skip-health-check: \"true\"") {
		t.Errorf("Deployment did not get the skip-health-check annotation:\n%s", out["deploy.yaml"])
	}
	if !strings.Contains(string(out["deploy.yaml"]), "existing: keep") {
		t.Errorf("Deployment lost its existing annotation:\n%s", out["deploy.yaml"])
	}
	if !strings.Contains(string(out["ds.yaml"]), "resources.gardener.cloud/skip-health-check") {
		t.Errorf("DaemonSet did not get the skip-health-check annotation:\n%s", out["ds.yaml"])
	}
	if strings.Contains(string(out["secret.yaml"]), "skip-health-check") {
		t.Errorf("Secret should NOT get the skip-health-check annotation:\n%s", out["secret.yaml"])
	}
}
