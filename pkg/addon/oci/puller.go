// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/registry"
)

// ChartPuller pulls Helm charts from OCI registries with local filesystem caching.
// It compares OCI manifest digests against cached versions to avoid unnecessary
// downloads. On pull failure, it falls back to cached charts if available.
type ChartPuller struct {
	cacheDir string
	mu       sync.Mutex
	log      logr.Logger
}

// PullResult contains the chart archive bytes and digest metadata.
type PullResult struct {
	// Archive is the raw .tgz chart bytes, suitable for chartrenderer.RenderArchive.
	Archive []byte
	// Digest is the OCI manifest digest (sha256:...).
	Digest string
}

// NewChartPuller creates a puller that caches charts under cacheDir.
func NewChartPuller(cacheDir string, log logr.Logger) (*ChartPuller, error) {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("create chart cache dir %s: %w", cacheDir, err)
	}
	return &ChartPuller{
		cacheDir: cacheDir,
		log:      log.WithName("oci-puller"),
	}, nil
}

// Pull fetches a chart from an OCI reference (e.g., "oci://registry/repo/chart").
// It compares the remote digest against the local cache:
//   - If digests match, returns cached chart bytes (no download).
//   - If digests differ or no cache exists, downloads and caches.
//   - On pull failure, returns cached chart if available.
func (p *ChartPuller) Pull(ctx context.Context, ociRef string, version string) (*PullResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Normalize the OCI reference
	ref := strings.TrimPrefix(ociRef, "oci://")
	if version != "" {
		ref = ref + ":" + version
	}

	key := cacheKey(ociRef, version)
	cachedDigest := readCachedDigest(p.cacheDir, key)

	// Create an OCI registry client
	client, err := registry.NewClient(
		registry.ClientOptEnableCache(true),
	)
	if err != nil {
		// Can't create client — fall back to cache
		p.log.Error(err, "Failed to create OCI registry client, trying cache", "ref", ref)
		return p.fallbackToCache(key, ref)
	}

	// Pull the chart
	result, err := client.Pull(ref)
	if err != nil {
		// Pull failed — fall back to cache
		p.log.Error(err, "Failed to pull chart from OCI, trying cache", "ref", ref)
		return p.fallbackToCache(key, ref)
	}

	// Compare digests
	remoteDigest := result.Manifest.Digest
	if cachedDigest == remoteDigest {
		p.log.Info("Chart digest unchanged, using cache", "ref", ref, "digest", remoteDigest)
		cached := readCachedArchive(p.cacheDir, key)
		if cached != nil {
			return &PullResult{Archive: cached, Digest: remoteDigest}, nil
		}
		// Cache file missing despite digest match — use pulled data
	}

	// New chart version — cache it
	archive := result.Chart.Data
	if err := writeCache(p.cacheDir, key, archive, remoteDigest); err != nil {
		p.log.Error(err, "Failed to write chart to cache", "ref", ref)
		// Non-fatal — return the pulled chart anyway
	}

	p.log.Info("Pulled and cached chart from OCI", "ref", ref, "digest", remoteDigest)
	return &PullResult{Archive: archive, Digest: remoteDigest}, nil
}

// GetCached returns cached chart bytes for the given OCI ref, or nil if not cached.
func (p *ChartPuller) GetCached(ociRef string, version string) []byte {
	key := cacheKey(ociRef, version)
	return readCachedArchive(p.cacheDir, key)
}

// fallbackToCache returns cached chart bytes or an error if no cache exists.
func (p *ChartPuller) fallbackToCache(key, ref string) (*PullResult, error) {
	cached := readCachedArchive(p.cacheDir, key)
	if cached != nil {
		digest := readCachedDigest(p.cacheDir, key)
		p.log.Info("Using cached chart (OCI pull failed)", "ref", ref, "cachedDigest", digest)
		return &PullResult{Archive: cached, Digest: digest}, nil
	}
	return nil, fmt.Errorf("chart %s not available: OCI pull failed and no cached version exists", ref)
}
