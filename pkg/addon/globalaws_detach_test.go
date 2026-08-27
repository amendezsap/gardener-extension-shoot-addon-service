package addon

import "testing"

// TestGlobalAWSDetachLists asserts both detach lists parse onto GlobalAWSConfig so the
// delete-only (IAMPoliciesDetachOnDelete) and aggressive (IAMPoliciesDetach) knobs are wired.
func TestGlobalAWSDetachLists(t *testing.T) {
	data := `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
defaultNamespace: managed-resources
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
  iamPoliciesDetachOnDelete:
    - external-agents-policy
  iamPoliciesDetach:
    - some-aggressive-policy
addons: []
`
	m, err := ReadManifestFromData(data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if m.GlobalAWS == nil {
		t.Fatal("GlobalAWS nil")
	}
	if got := m.GlobalAWS.IAMPoliciesDetachOnDelete; len(got) != 1 || got[0] != "external-agents-policy" {
		t.Fatalf("IAMPoliciesDetachOnDelete = %v", got)
	}
	if got := m.GlobalAWS.IAMPoliciesDetach; len(got) != 1 || got[0] != "some-aggressive-policy" {
		t.Fatalf("IAMPoliciesDetach = %v", got)
	}
	if got := m.GlobalAWS.IAMPolicies; len(got) != 1 || got[0] != "CloudWatchAgentServerPolicy" {
		t.Fatalf("IAMPolicies = %v", got)
	}
}

// TestGlobalAWSDetachListsOmittedEmpty asserts both lists default empty when absent.
func TestGlobalAWSDetachListsOmittedEmpty(t *testing.T) {
	data := `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
globalAWS:
  iamPolicies:
    - AmazonSSMManagedInstanceCore
addons: []
`
	m, err := ReadManifestFromData(data)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(m.GlobalAWS.IAMPoliciesDetachOnDelete) != 0 || len(m.GlobalAWS.IAMPoliciesDetach) != 0 {
		t.Fatalf("expected empty detach lists, got onDelete=%v detach=%v",
			m.GlobalAWS.IAMPoliciesDetachOnDelete, m.GlobalAWS.IAMPoliciesDetach)
	}
}
