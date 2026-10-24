// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAIToolDefinitionsAreValid checks that every tool this app declares passes the server's own
// validity check (model.AIToolDefinition.IsValid), is a read tool, and that names aren't
// duplicated.
func TestAIToolDefinitionsAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range aiToolDefinitions() {
		t.Run(d.Name, func(t *testing.T) {
			assert.Nil(t, d.IsValid())
			assert.Equal(t, model.AIToolKindRead, d.Kind, "this app's tools are all read-only")
			assert.False(t, seen[d.Name], "duplicate tool name")
			seen[d.Name] = true
		})
	}
	assert.Len(t, seen, 5)
}

// TestOnActivateRegistersValidTools checks that OnActivate calls RegisterAITools with exactly
// this app's definitions, every one of them already valid.
func TestOnActivateRegistersValidTools(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	api.On("RegisterAITools", mock.MatchedBy(func(tools []*model.AIToolDefinition) bool {
		if len(tools) != 5 {
			return false
		}
		for _, tool := range tools {
			if tool.IsValid() != nil {
				return false
			}
		}
		return true
	})).Return(nil)

	p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
	require.NoError(t, p.OnActivate())
	assert.NotNil(t, p.toolLimiter)
	assert.NotNil(t, p.renderer)
}

func toolExecution(tool, userID, channelID, teamID string, args any) *model.AIToolExecution {
	raw, _ := json.Marshal(args)
	return &model.AIToolExecution{
		CallId:    model.NewId(),
		Tool:      manifest.Id + "." + tool,
		Arguments: raw,
		UserId:    userID,
		ChannelId: channelID,
		TeamId:    teamID,
	}
}

func TestExecuteAIToolNotConfigured(t *testing.T) {
	env := newTestEnv(t, "", configuration{})
	env.p.configuration.ServiceAccountToken = ""
	env.expectLogs(nil)
	result := env.p.ExecuteAITool(nil, toolExecution(toolSearchDashboards, "u1", "c1", "", map[string]string{"query": ""}))
	require.NotEmpty(t, result.Error)
	assert.Empty(t, result.Text)
}

func TestExecuteAIToolSearchDashboards(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`[{"uid":"node-www","title":"Node overview","folderTitle":"Infra","url":"/d/node-www/node-overview"}]`))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolSearchDashboards, "u1", "c1", "", map[string]string{"query": "node"}))
	require.Empty(t, result.Error)
	assert.Contains(t, result.Text, "Node overview")
	assert.Contains(t, result.Text, "node-www")
	assert.Contains(t, result.Text, fake.URL+"/d/node-www/node-overview")
	assert.NotContains(t, result.Text, testToken)
}

func TestExecuteAIToolGetDashboard(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"dashboard":{"uid":"node-www","title":"Node overview","panels":[
			{"id":2,"title":"CPU","type":"timeseries","targets":[{"refId":"A","expr":"100 - avg(rate(node_cpu_seconds_total{mode=\"idle\"}[5m]))*100"}]}
		]}}`))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolGetDashboard, "u1", "c1", "", map[string]string{"uid": "node-www"}))
	require.Empty(t, result.Error)
	assert.Contains(t, result.Text, "Node overview")
	assert.Contains(t, result.Text, "CPU")
	assert.Contains(t, result.Text, "node_cpu_seconds_total")
	assert.Contains(t, result.Text, fake.URL+"/d/node-www")
}

func dsQueryFrame(labels map[string]string, times, values []float64) map[string]any {
	toNumbers := func(fs []float64) []json.Number {
		out := make([]json.Number, len(fs))
		for i, f := range fs {
			out[i] = json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
		return out
	}
	return map[string]any{
		"schema": map[string]any{
			"fields": []map[string]any{
				{"name": "Time", "type": "time"},
				{"name": "Value", "type": "number", "labels": labels},
			},
		},
		"data": map[string]any{"values": []any{toNumbers(times), toNumbers(values)}},
	}
}

func TestExecuteAIToolQueryMetrics(t *testing.T) {
	// 150 points, so downsampling to 120 is exercised end to end.
	n := 150
	times := make([]float64, n)
	values := make([]float64, n)
	for i := range n {
		times[i] = float64(i * 1000)
		values[i] = float64(i)
	}
	frame := dsQueryFrame(map[string]string{"instance": "www-1"}, times, values)

	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		queries := body["queries"].([]any)
		q := queries[0].(map[string]any)
		assert.Equal(t, "rate(node_cpu[5m])", q["expr"])
		ds := q["datasource"].(map[string]any)
		assert.Equal(t, "prometheus", ds["uid"])

		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{frame}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolQueryMetrics, "u1", "c1", "", map[string]string{"expr": "rate(node_cpu[5m])"}))
	require.Empty(t, result.Error)
	assert.Contains(t, result.Text, "instance=www-1")
	assert.Contains(t, result.Text, "last=149")
	assert.Contains(t, result.Text, "min=0")
	assert.Contains(t, result.Text, "max=149")
	assert.Contains(t, result.Text, fake.URL+"/explore?left=")
	assert.NotContains(t, result.Text, testToken)
}

func TestExecuteAIToolQueryMetricsRequiresExpr(t *testing.T) {
	env := newTestEnv(t, "http://unused.invalid", configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)
	result := env.p.ExecuteAITool(nil, toolExecution(toolQueryMetrics, "u1", "c1", "", map[string]string{}))
	assert.NotEmpty(t, result.Error)
}

func TestExecuteAIToolPanelData(t *testing.T) {
	frame := dsQueryFrame(map[string]string{"instance": "www-1"}, []float64{0, 1000}, []float64{3, 7})
	dashboard := `{"dashboard":{"uid":"node-www","title":"Node overview","panels":[
		{"id":2,"title":"CPU","type":"timeseries","datasource":{"uid":"prometheus"},"targets":[{"refId":"A","expr":"cpu"}]}
	]}}`

	for _, tc := range []struct {
		name          string
		pluginsBody   string
		wantInstalled bool
	}{
		{"renderer installed", `[{"id":"grafana-image-renderer"}]`, true},
		{"renderer not installed", `[{"id":"other"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/"):
					_, _ = w.Write([]byte(dashboard))
				case r.URL.Path == "/api/ds/query":
					resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{frame}}}}
					encoded, _ := json.Marshal(resp)
					_, _ = w.Write(encoded)
				case r.URL.Path == "/api/plugins":
					_, _ = w.Write([]byte(tc.pluginsBody))
				default:
					t.Fatalf("unexpected request to %s", r.URL.Path)
				}
			})
			env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
			env.allowChannel("u1", "c1", "")
			env.expectLogs(nil)

			result := env.p.ExecuteAITool(nil, toolExecution(toolPanelData, "u1", "c1", "", map[string]string{"dashboard_uid": "node-www", "panel_id": "2"}))
			require.Empty(t, result.Error)
			assert.Contains(t, result.Text, "CPU")
			assert.Contains(t, result.Text, "last=7")
			assert.Contains(t, result.Text, fake.URL+"/d/node-www?viewPanel=2")
			if tc.wantInstalled {
				assert.Contains(t, result.Text, "Rendered panel image: "+fake.URL)
				assert.Contains(t, result.Text, "render/d-solo/node-www")
			} else {
				assert.Contains(t, result.Text, "Rendered panel image: not available")
			}
			assert.NotContains(t, result.Text, testToken)
		})
	}
}

func TestExecuteAIToolPanelDataUnknownPanel(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"dashboard":{"uid":"node-www","title":"Node overview","panels":[]}}`))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)
	result := env.p.ExecuteAITool(nil, toolExecution(toolPanelData, "u1", "c1", "", map[string]string{"dashboard_uid": "node-www", "panel_id": "99"}))
	assert.NotEmpty(t, result.Error)
}

func TestExecuteAIToolListAlerts(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"groups":[{"name":"default","rules":[
			{"name":"Watchdog","state":"firing","health":"ok"},
			{"name":"InstanceDown","state":"inactive","health":"ok"}
		]}]}}`))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	all := env.p.ExecuteAITool(nil, toolExecution(toolListAlerts, "u1", "c1", "", map[string]string{}))
	require.Empty(t, all.Error)
	assert.Contains(t, all.Text, "Watchdog")
	assert.Contains(t, all.Text, "InstanceDown")

	firing := env.p.ExecuteAITool(nil, toolExecution(toolListAlerts, "u1", "c1", "", map[string]string{"state": "firing"}))
	require.Empty(t, firing.Error)
	assert.Contains(t, firing.Text, "Watchdog")
	assert.NotContains(t, firing.Text, "InstanceDown")
}

// TestExecuteAIToolCommunityGate checks that a tool call from a community the settings don't
// allow is refused before it ever reaches Grafana.
func TestExecuteAIToolCommunityGate(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	api.On("GetChannel", "c1").Return(&model.Channel{Id: "c1", TeamId: "team1"}, nil)
	api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "other-team"}, nil)

	p := &Plugin{
		MattermostPlugin: plugin.MattermostPlugin{API: api},
		configuration:    &configuration{GrafanaURL: "http://unused.invalid", ServiceAccountToken: testToken, AllCommunities: false, AllowedCommunities: "ops-team"},
		toolLimiter:      newToolLimiter(),
		renderer:         &rendererDetector{},
	}
	result := p.ExecuteAITool(nil, toolExecution(toolSearchDashboards, "u1", "c1", "", map[string]string{"query": ""}))
	require.NotEmpty(t, result.Error)
	assert.Empty(t, result.Text)
}

// TestExecuteAIToolRateLimited checks that a person who has used up their burst gets a clear
// refusal rather than a Grafana request.
func TestExecuteAIToolRateLimited(t *testing.T) {
	env := newTestEnv(t, "http://unused.invalid", configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)
	now := time.Now()
	for range toolRateBurst {
		assert.True(t, env.p.toolLimiter.allow("u1", now))
	}
	result := env.p.ExecuteAITool(nil, toolExecution(toolSearchDashboards, "u1", "c1", "", map[string]string{"query": ""}))
	assert.NotEmpty(t, result.Error)
}

// TestExecuteAIToolNeverLogsOrLeaksToken checks the token never appears in the result (text or
// error) or in anything logged, even when Grafana's own error response tries to echo it back.
func TestExecuteAIToolNeverLogsOrLeaksToken(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "token %q rejected", r.Header.Get("Authorization"))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")

	var logged []any
	env.expectLogs(func(args ...any) { logged = append(logged, args...) })

	result := env.p.ExecuteAITool(nil, toolExecution(toolSearchDashboards, "u1", "c1", "", map[string]string{"query": ""}))
	require.NotEmpty(t, result.Error)
	assert.NotContains(t, result.Error, testToken)
	assert.NotContains(t, result.Text, testToken)
	for _, v := range logged {
		if s, ok := v.(string); ok {
			assert.NotContains(t, s, testToken)
		}
	}
}
