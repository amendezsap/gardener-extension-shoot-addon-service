// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package addon

import (
	"testing"
)

func TestReadManifestFromData(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
		addons  int
	}{
		{
			name:    "empty data",
			data:    "",
			wantErr: true,
		},
		{
			name:    "missing apiVersion",
			data:    "kind: AddonManifest\naddons: []",
			wantErr: true,
		},
		{
			name:    "missing kind",
			data:    "apiVersion: addons.gardener.cloud/v1alpha1\naddons: []",
			wantErr: true,
		},
		{
			name: "valid manifest with OCI addon",
			data: `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
defaultNamespace: managed-resources
addons:
  - name: fluent-bit
    chart:
      oci: oci://registry.example.com/charts/fluent-bit
      version: "0.56.0"
    enabled: true
    target: global
`,
			wantErr: false,
			addons:  1,
		},
		{
			name: "OCI addon missing version",
			data: `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
addons:
  - name: test
    chart:
      oci: oci://registry.example.com/charts/test
    enabled: true
`,
			wantErr: true,
		},
		{
			name: "addon missing name",
			data: `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
addons:
  - chart:
      oci: oci://registry.example.com/charts/test
      version: "1.0"
`,
			wantErr: true,
		},
		{
			name: "multiple addons",
			data: `
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
defaultNamespace: managed-resources
addons:
  - name: fluent-bit
    chart:
      oci: oci://registry.example.com/charts/fluent-bit
      version: "0.56.0"
    enabled: true
  - name: container-report
    chart:
      oci: oci://registry.example.com/charts/list-containers
      version: "v0.1.7"
    enabled: true
`,
			wantErr: false,
			addons:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ReadManifestFromData(tt.data)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if len(m.Addons) != tt.addons {
				t.Errorf("expected %d addons, got %d", tt.addons, len(m.Addons))
			}
		})
	}
}

func TestAddonTarget(t *testing.T) {
	tests := []struct {
		target       AddonTarget
		deploysShoot bool
		deploysSeed  bool
	}{
		{"", true, false},           // default = shoot
		{AddonTargetShoot, true, false},
		{AddonTargetSeed, false, true},
		{AddonTargetGlobal, true, true},
	}

	for _, tt := range tests {
		addon := &Addon{Target: tt.target}
		if addon.DeploysToShoot() != tt.deploysShoot {
			t.Errorf("target %q: DeploysToShoot() = %v, want %v", tt.target, addon.DeploysToShoot(), tt.deploysShoot)
		}
		if addon.DeploysToSeed() != tt.deploysSeed {
			t.Errorf("target %q: DeploysToSeed() = %v, want %v", tt.target, addon.DeploysToSeed(), tt.deploysSeed)
		}
	}
}

func TestGetManagedResourceName(t *testing.T) {
	tests := []struct {
		name     string
		mrName   string
		expected string
	}{
		{"fluent-bit", "", "fluent-bit"},
		{"fluent-bit", "fluent-bit", "fluent-bit"},
		{"test", "custom-name", "custom-name"},
	}

	for _, tt := range tests {
		addon := &Addon{Name: tt.name, ManagedResourceName: tt.mrName}
		if got := addon.GetManagedResourceName(); got != tt.expected {
			t.Errorf("GetManagedResourceName(%q, %q) = %q, want %q", tt.name, tt.mrName, got, tt.expected)
		}
	}
}

func TestGetSeedManagedResourceName(t *testing.T) {
	addon := &Addon{Name: "fluent-bit"}
	expected := "seed-fluent-bit"
	if got := addon.GetSeedManagedResourceName(); got != expected {
		t.Errorf("GetSeedManagedResourceName() = %q, want %q", got, expected)
	}
}

func TestValidateRemote(t *testing.T) {
	tests := []struct {
		name    string
		addon   Addon
		wantErr bool
	}{
		{
			name:    "valid OCI",
			addon:   Addon{Name: "test", Chart: ChartSource{OCI: "oci://reg/chart", Version: "1.0"}},
			wantErr: false,
		},
		{
			name:    "OCI missing version",
			addon:   Addon{Name: "test", Chart: ChartSource{OCI: "oci://reg/chart"}},
			wantErr: true,
		},
		{
			name:    "missing name",
			addon:   Addon{Chart: ChartSource{OCI: "oci://reg/chart", Version: "1.0"}},
			wantErr: true,
		},
		{
			name:    "no chart source",
			addon:   Addon{Name: "test"},
			wantErr: true,
		},
		{
			name:    "valid path",
			addon:   Addon{Name: "test", Chart: ChartSource{Path: "test/chart"}},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.addon.ValidateRemote()
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRemote() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
