// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// cacheKey returns a safe directory name for the given OCI reference and version.
func cacheKey(ociRef, version string) string {
	h := sha256.Sum256([]byte(ociRef + ":" + version))
	return hex.EncodeToString(h[:8])
}

// cachePath returns the directory path for a cached chart.
func cachePath(cacheDir, key string) string {
	return filepath.Join(cacheDir, key)
}

// readCachedDigest reads the stored digest for a cached chart.
// Returns empty string if no cached digest exists.
func readCachedDigest(cacheDir, key string) string {
	data, err := os.ReadFile(filepath.Join(cachePath(cacheDir, key), "digest"))
	if err != nil {
		return ""
	}
	return string(data)
}

// readCachedArchive reads the cached chart archive bytes.
// Returns nil if no cached archive exists.
func readCachedArchive(cacheDir, key string) []byte {
	data, err := os.ReadFile(filepath.Join(cachePath(cacheDir, key), "chart.tgz"))
	if err != nil {
		return nil
	}
	return data
}

// writeCache writes the chart archive and digest to the cache directory.
func writeCache(cacheDir, key string, archive []byte, digest string) error {
	dir := cachePath(cacheDir, key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "chart.tgz"), archive, 0600); err != nil {
		return fmt.Errorf("write chart archive: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "digest"), []byte(digest), 0600); err != nil {
		return fmt.Errorf("write digest: %w", err)
	}

	return nil
}
