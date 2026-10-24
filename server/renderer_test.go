// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRendererDetectorCaches(t *testing.T) {
	calls := 0
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`[{"id":"grafana-image-renderer"}]`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	d := &rendererDetector{}
	now := time.Now()

	assert.True(t, d.isInstalled(context.Background(), client, now))
	assert.True(t, d.isInstalled(context.Background(), client, now.Add(time.Second)))
	assert.Equal(t, 1, calls, "the second call, within the TTL, should reuse the cached answer")

	assert.True(t, d.isInstalled(context.Background(), client, now.Add(rendererCacheTTL+time.Second)))
	assert.Equal(t, 2, calls, "past the TTL, it should ask Grafana again")
}
