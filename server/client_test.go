// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchDashboardsRequestShape(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/search", r.URL.Path)
		assert.Equal(t, "node", r.URL.Query().Get("query"))
		assert.Equal(t, "dash-db", r.URL.Query().Get("type"))
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"uid":"node-www","title":"Node overview","folderTitle":"General","url":"/d/node-www/node-overview"}]`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	hits, err := client.searchDashboards(context.Background(), "node")
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "node-www", hits[0].UID)
	assert.Equal(t, "Node overview", hits[0].Title)
}

func TestGetDashboardRequestShape(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/dashboards/uid/node-www", r.URL.Path)
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"dashboard":{"uid":"node-www","title":"Node overview","panels":[
			{"id":2,"title":"CPU","type":"timeseries","datasource":{"uid":"prometheus"},"targets":[{"refId":"A","expr":"rate(node_cpu[5m])"}]}
		]}}`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	resp, err := client.getDashboard(context.Background(), "node-www")
	require.NoError(t, err)
	assert.Equal(t, "Node overview", resp.Dashboard.Title)
	require.Len(t, resp.Dashboard.Panels, 1)
	assert.Equal(t, "2", resp.Dashboard.Panels[0].ID.String())
	assert.Equal(t, "rate(node_cpu[5m])", resp.Dashboard.Panels[0].Targets[0]["expr"])
}

func TestQueryDSRequestShapeAndAuth(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/ds/query", r.URL.Path)
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		queries, _ := body["queries"].([]any)
		require.Len(t, queries, 1)
		q, _ := queries[0].(map[string]any)
		assert.Equal(t, "up", q["expr"])
		assert.Equal(t, "now-1h", body["from"])
		assert.Equal(t, "now", body["to"])
		_, _ = w.Write([]byte(`{"results":{"A":{"frames":[]}}}`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	_, err := client.queryDS(context.Background(), "query_metrics", []dsQuery{{RefID: "A", Extra: map[string]any{"expr": "up"}}}, "", "")
	require.NoError(t, err)
}

func TestListAlertRules(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/prometheus/grafana/api/v1/rules", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":{"groups":[{"name":"default","rules":[
			{"name":"Watchdog","state":"firing","health":"ok","alerts":[{"labels":{"severity":"none"},"state":"firing"}]},
			{"name":"InstanceDown","state":"inactive","health":"ok"}
		]}]}}`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	rules, err := client.listAlertRules(context.Background())
	require.NoError(t, err)
	require.Len(t, rules, 2)
	assert.Equal(t, "Watchdog", rules[0].Name)
	assert.Equal(t, "firing", rules[0].State)
	assert.Equal(t, "none", rules[0].Labels["severity"])
	assert.Equal(t, "inactive", rules[1].State)
}

func TestHasImageRenderer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		installed bool
	}{
		{"installed", `[{"id":"grafana-image-renderer"},{"id":"other"}]`, true},
		{"not installed", `[{"id":"other"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/plugins", r.URL.Path)
				_, _ = w.Write([]byte(tc.body))
			})
			client := newGrafanaClient(fake.URL, testToken)
			assert.Equal(t, tc.installed, client.hasImageRenderer(context.Background()))
		})
	}
}

// TestHasImageRendererFailsClosed checks that a Grafana error (for instance, because the service
// account can't list plugins) is treated as "not installed", never as an error that stops
// panel_data from giving its data.
func TestHasImageRendererFailsClosed(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	client := newGrafanaClient(fake.URL, testToken)
	assert.False(t, client.hasImageRenderer(context.Background()))
}

// TestOversizedResponseCapped checks that a Grafana response far larger than maxResponseSize is
// truncated rather than read in full: the truncated body fails to parse as JSON, so the call
// fails cleanly instead of consuming unbounded memory.
func TestOversizedResponseCapped(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		hit := `{"uid":"x","title":"` + strings.Repeat("a", 200) + `"},`
		written := 1
		for written < maxResponseSize+(1<<20) { // write 1 MiB past the cap
			n, _ := w.Write([]byte(hit))
			written += n
		}
		_, _ = w.Write([]byte(`{"uid":"last","title":"last"}]`))
	})
	client := newGrafanaClient(fake.URL, testToken)
	_, err := client.searchDashboards(context.Background(), "")
	require.Error(t, err)
}

// TestContextDeadlineBoundsRequests checks that a request to a slow Grafana is cut off by the
// context passed in, not just by the client's own fixed timeout: ExecuteAITool relies on this to
// keep a whole tool call (even panel_data's two requests) within toolTimeout.
func TestContextDeadlineBoundsRequests(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	})
	client := newGrafanaClient(fake.URL, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := client.searchDashboards(ctx, "")
	elapsed := time.Since(started)

	require.Error(t, err)
	assert.Less(t, elapsed, 1*time.Second, "the request should have been cut off by the context deadline, not run to completion")
}

// TestErrorsNeverContainToken checks that none of this app's own error paths ever put the
// service account token in the message they return (even if Grafana's own error body were to
// echo it back, which a misbehaving or compromised Grafana could do).
func TestErrorsNeverContainToken(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "unauthorized: the token %q was rejected", r.Header.Get("Authorization"))
	})
	client := newGrafanaClient(fake.URL, testToken)
	_, err := client.searchDashboards(context.Background(), "")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}
