// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCacheKey(t *testing.T) {
	k1 := cacheKey("oci://reg/chart", "1.0")
	k2 := cacheKey("oci://reg/chart", "1.0")
	k3 := cacheKey("oci://reg/chart", "2.0")

	if k1 != k2 {
		t.Errorf("same input should produce same key: %s != %s", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different version should produce different key: %s == %s", k1, k3)
	}
	if len(k1) != 16 {
		t.Errorf("key should be 16 hex chars, got %d: %s", len(k1), k1)
	}
}

func TestWriteAndReadCache(t *testing.T) {
	dir := t.TempDir()
	key := "testkey"
	archive := []byte("chart-data-here")
	digest := "sha256:abc123"

	if err := writeCache(dir, key, archive, digest); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

	// Verify files exist
	if _, err := os.Stat(filepath.Join(dir, key, "chart.tgz")); err != nil {
		t.Errorf("chart.tgz not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, key, "digest")); err != nil {
		t.Errorf("digest not found: %v", err)
	}

	// Read back
	gotDigest := readCachedDigest(dir, key)
	if gotDigest != digest {
		t.Errorf("readCachedDigest = %q, want %q", gotDigest, digest)
	}

	gotArchive := readCachedArchive(dir, key)
	if string(gotArchive) != string(archive) {
		t.Errorf("readCachedArchive = %q, want %q", gotArchive, archive)
	}
}

func TestReadCacheNotFound(t *testing.T) {
	dir := t.TempDir()

	digest := readCachedDigest(dir, "nonexistent")
	if digest != "" {
		t.Errorf("expected empty digest for nonexistent key, got %q", digest)
	}

	archive := readCachedArchive(dir, "nonexistent")
	if archive != nil {
		t.Errorf("expected nil archive for nonexistent key, got %v", archive)
	}
}
