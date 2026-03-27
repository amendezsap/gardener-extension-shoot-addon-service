// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
)

func TestIsAddonEnabled(t *testing.T) {
	trueVal := true
	falseVal := false

	tests := []struct {
		name            string
		cfg             AddonServiceConfig
		addonName       string
		manifestEnabled bool
		want            bool
	}{
		{"no overrides, manifest true", AddonServiceConfig{}, "test", true, true},
		{"no overrides, manifest false", AddonServiceConfig{}, "test", false, false},
		{"override true", AddonServiceConfig{Addons: map[string]AddonOverride{"test": {Enabled: &trueVal}}}, "test", false, true},
		{"override false", AddonServiceConfig{Addons: map[string]AddonOverride{"test": {Enabled: &falseVal}}}, "test", true, false},
		{"different addon", AddonServiceConfig{Addons: map[string]AddonOverride{"other": {Enabled: &falseVal}}}, "test", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsAddonEnabled(tt.addonName, tt.manifestEnabled)
			if got != tt.want {
				t.Errorf("IsAddonEnabled(%q, %v) = %v, want %v", tt.addonName, tt.manifestEnabled, got, tt.want)
			}
		})
	}
}

func TestIsOverrideMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"", false},
		{"merge", false},
		{"override", true},
		{"Override", true},
		{"OVERRIDE", true},
	}

	for _, tt := range tests {
		o := &AddonOverride{ValuesMode: tt.mode}
		if got := o.IsOverrideMode(); got != tt.want {
			t.Errorf("IsOverrideMode(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}
