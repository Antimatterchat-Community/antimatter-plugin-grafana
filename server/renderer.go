// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"context"
	"sync"
	"time"
)

// rendererCacheTTL bounds how long a renderer detection is reused before checking again: long
// enough that panel_data doesn't call GET /api/plugins on every tool call, short enough that
// installing or removing grafana-image-renderer takes effect without restarting the app.
const rendererCacheTTL = 5 * time.Minute

// rendererDetector caches whether the grafana-image-renderer plugin is installed, so panel_data
// doesn't have to ask Grafana on every call.
type rendererDetector struct {
	mu        sync.Mutex
	checkedAt time.Time
	installed bool
}

// installed tells whether the renderer plugin is installed, asking Grafana again if the cached
// answer is older than rendererCacheTTL.
func (d *rendererDetector) isInstalled(ctx context.Context, client *grafanaClient, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.checkedAt) < rendererCacheTTL {
		return d.installed
	}
	d.installed = client.hasImageRenderer(ctx)
	d.checkedAt = now
	return d.installed
}
