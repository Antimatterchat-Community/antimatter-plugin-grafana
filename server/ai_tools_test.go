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
	assert.Len(t, seen, 7)
}

// TestOnActivateRegistersValidTools checks that OnActivate calls RegisterAITools with exactly
// this app's definitions, every one of them already valid.
func TestOnActivateRegistersValidTools(t *testing.T) {
	api := &plugintest.API{}
	defer api.AssertExpectations(t)
	api.On("RegisterAITools", mock.MatchedBy(func(tools []*model.AIToolDefinition) bool {
		if len(tools) != 7 {
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
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 1)
	att := out.Attachments[0]
	assert.Contains(t, att.Title, "instance=www-1")
	assert.Equal(t, "149", fieldValue(t, att, "Now"))
	assert.Equal(t, "0", fieldValue(t, att, "Low"))
	assert.Equal(t, "149", fieldValue(t, att, "Peak"))
	assert.Equal(t, fake.URL+"/explore?left=", att.TitleLink[:len(fake.URL+"/explore?left=")])
	require.NotNil(t, att.Chart)
	assert.LessOrEqual(t, len(att.Chart.Values), 60)
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
			out := decodeToolResult(t, result)
			require.Len(t, out.Attachments, 1)
			att := out.Attachments[0]
			assert.Contains(t, att.Title, "CPU")
			assert.Equal(t, "7", fieldValue(t, att, "Now"))
			assert.Equal(t, fake.URL+"/d/node-www?viewPanel=2", att.TitleLink)
			if tc.wantInstalled {
				assert.Contains(t, att.ImageURL, fake.URL)
				assert.Contains(t, att.ImageURL, "render/d-solo/node-www")
			} else {
				assert.Empty(t, att.ImageURL)
			}
			assert.NotContains(t, result.Text, testToken)
		})
	}
}

// TestExecuteAIToolPanelDataUsesPanelUnit checks panel_data reads the panel's own Grafana unit
// code (fieldConfig.defaults.unit) and formats its result's fields with it, and that a
// "percentunit" (a 0..1 fraction) is scaled to a percentage before formatting.
func TestExecuteAIToolPanelDataUsesPanelUnit(t *testing.T) {
	frame := dsQueryFrame(map[string]string{"instance": "www-1"}, []float64{0, 1000}, []float64{0.25, 0.75})
	dashboard := `{"dashboard":{"uid":"node-www","title":"Node overview","panels":[
		{"id":2,"title":"CPU","type":"timeseries","datasource":{"uid":"prometheus"},
		 "fieldConfig":{"defaults":{"unit":"percentunit"}},
		 "targets":[{"refId":"A","expr":"cpu"}]}
	]}}`
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/"):
			_, _ = w.Write([]byte(dashboard))
		case r.URL.Path == "/api/ds/query":
			resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{frame}}}}
			encoded, _ := json.Marshal(resp)
			_, _ = w.Write(encoded)
		case r.URL.Path == "/api/plugins":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolPanelData, "u1", "c1", "", map[string]string{"dashboard_uid": "node-www", "panel_id": "2"}))
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 1)
	assert.Equal(t, "75.0%", fieldValue(t, out.Attachments[0], "Now"))
	assert.Equal(t, "25.0%", fieldValue(t, out.Attachments[0], "Low"))
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

// TestExecuteAIToolListAlertsMapping checks the attachments list_alerts gives: a rule's state
// colours the attachment (firing danger, pending amber, inactive green), its severity label
// shows (defaulting to "info" when it has none), its target and since come from its active
// alert, and an empty result is a single calm attachment.
func TestExecuteAIToolListAlertsMapping(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"groups":[{"name":"default","rules":[
			{"name":"HighCPU","state":"firing","health":"ok","alerts":[
				{"labels":{"instance":"www-1","severity":"critical"},"annotations":{"summary":"CPU above 90%"},"activeAt":"2026-10-24T17:55:00Z"}
			]},
			{"name":"SlowDisk","state":"pending","health":"ok","alerts":[
				{"labels":{"job":"node"},"annotations":{},"activeAt":"2026-10-24T18:00:00Z"}
			]},
			{"name":"Watchdog","state":"inactive","health":"ok"}
		]}]}}`))
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolListAlerts, "u1", "c1", "", map[string]string{}))
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 3)

	highCPU := out.Attachments[0]
	assert.Equal(t, "danger", highCPU.Color)
	assert.Equal(t, "critical", fieldValue(t, highCPU, "Severity"))
	assert.Equal(t, "www-1", fieldValue(t, highCPU, "Target"))
	assert.Equal(t, "2026-10-24 17:55 UTC", fieldValue(t, highCPU, "Since"))
	assert.Equal(t, "CPU above 90%", highCPU.Text)

	slowDisk := out.Attachments[1]
	assert.Equal(t, "warning", slowDisk.Color)
	// No severity label defaults to "info", never left blank.
	assert.Equal(t, "info", fieldValue(t, slowDisk, "Severity"))
	assert.Equal(t, "node", fieldValue(t, slowDisk, "Target"), "falls back to the job when there's no instance")

	watchdog := out.Attachments[2]
	assert.Equal(t, "good", watchdog.Color)
	assert.Equal(t, "info", fieldValue(t, watchdog, "Severity"))

	none := env.p.ExecuteAITool(nil, toolExecution(toolListAlerts, "u1", "c1", "", map[string]string{"state": "bogus-state-matching-nothing"}))
	noneOut := decodeToolResult(t, none)
	require.Len(t, noneOut.Attachments, 1)
	assert.Equal(t, "good", noneOut.Attachments[0].Color)
}

// TestExecuteAIToolHostMetricsUnsafePublicURLDropped checks an admin-configured public URL that
// isn't http(s) never reaches the attachment's link.
func TestExecuteAIToolHostMetricsUnsafePublicURLDropped(t *testing.T) {
	frame := dsQueryFrame(map[string]string{"instance": "www-1"}, []float64{0, 1000}, []float64{1, 2})
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{frame}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true, GrafanaPublicURL: "javascript:alert(1)"})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "load"}))
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 1)
	assert.Empty(t, out.Attachments[0].TitleLink, "an unsafe public URL must never reach the attachment's link")
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

// dsQueryBody decodes a /api/ds/query request body, once per request (http.Request.Body can only
// be read once), for the host_metrics tests below.
func dsQueryBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
	return body
}

// exprOf decodes a /api/ds/query request body and returns its first query's "expr", for the
// host_metrics tests below to tell which query of a call (the metric itself, or the nodename
// fallback probe) a given request is.
func exprOf(t *testing.T, r *http.Request) string {
	t.Helper()
	queries := dsQueryBody(t, r)["queries"].([]any)
	return queries[0].(map[string]any)["expr"].(string)
}

// TestExecuteAIToolHostMetrics checks that host_metrics builds its own PromQL (no expr argument
// exists for this tool) and returns the same result shape as query_metrics, titled for the
// person.
func TestExecuteAIToolHostMetrics(t *testing.T) {
	frame := dsQueryFrame(map[string]string{"instance": "www-1"}, []float64{0, 1000}, []float64{1.5, 2.5})
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		expr := exprOf(t, r)
		assert.Contains(t, expr, "node_load1")
		assert.Contains(t, expr, `instance=~"www-1(:[0-9]+)?"`)
		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{frame}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "load"}))
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 1)
	att := out.Attachments[0]
	assert.Contains(t, att.Title, "Load on www-1 (last hour)")
	assert.Equal(t, "2.5", fieldValue(t, att, "Now"))
	assert.Equal(t, "1.5", fieldValue(t, att, "Low"))
	assert.Equal(t, "2.5", fieldValue(t, att, "Peak"))
	require.NotNil(t, att.Chart)
	assert.NotEmpty(t, att.Chart.Values)
	assert.NotContains(t, result.Text, testToken)
}

// TestExecuteAIToolHostMetricsAliasesAndWindow checks a metric alias (ram) and an explicit time
// range both reach the tool and its title.
func TestExecuteAIToolHostMetricsAliasesAndWindow(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		body := dsQueryBody(t, r)
		expr := body["queries"].([]any)[0].(map[string]any)["expr"].(string)
		assert.Equal(t, "now-6h", body["from"])
		if !strings.Contains(expr, "node_uname_info") {
			// The main query, not the nodename fallback probe it triggers once this returns no
			// data below.
			assert.Contains(t, expr, "node_memory_MemAvailable_bytes")
		}
		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "ram", "from": "now-6h"}))
	require.Empty(t, result.Error)
	assert.Contains(t, result.Text, "Memory on www-1 (last 6 hours)")
	assert.Contains(t, result.Text, "no data")
}

func TestExecuteAIToolHostMetricsUnknownMetric(t *testing.T) {
	env := newTestEnv(t, "http://unused.invalid", configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)
	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "bogus"}))
	assert.NotEmpty(t, result.Error)
}

func TestExecuteAIToolHostMetricsRequiresHost(t *testing.T) {
	env := newTestEnv(t, "http://unused.invalid", configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)
	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "", "metric": "cpu"}))
	assert.NotEmpty(t, result.Error)
}

// TestExecuteAIToolHostMetricsNodenameFallback checks that when a host doesn't match the instance
// label at all (here the person asked for "web1", node-exporter's own hostname, not how
// Prometheus scrapes it), host_metrics resolves it via node_uname_info's nodename label to the
// actual instance ("www-1:9100") and re-queries with that, rather than reporting no data.
func TestExecuteAIToolHostMetricsNodenameFallback(t *testing.T) {
	nodenameFrame := map[string]any{
		"schema": map[string]any{"fields": []map[string]any{
			{"name": "Value", "type": "number", "labels": map[string]string{"instance": "www-1:9100", "nodename": "web1"}},
		}},
		"data": map[string]any{"values": []any{[]json.Number{"0"}, []json.Number{"1"}}},
	}
	dataFrame := dsQueryFrame(map[string]string{"instance": "www-1:9100"}, []float64{0, 1000}, []float64{10, 20})

	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		expr := exprOf(t, r)
		var resp map[string]any
		switch {
		case strings.Contains(expr, "node_uname_info"):
			assert.Contains(t, expr, `nodename=~"web1"`)
			resp = map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{nodenameFrame}}}}
		case strings.Contains(expr, `instance=~"web1(:[0-9]+)?"`):
			// The first, direct attempt: nothing matches "web1" as an instance.
			resp = map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{}}}}
		case strings.Contains(expr, `instance=~"^(www-1:9100)$"`):
			// The retry, with the instance resolved via nodename.
			resp = map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{dataFrame}}}}
		default:
			t.Fatalf("unexpected expr: %s", expr)
		}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "web1", "metric": "cpu"}))
	out := decodeToolResult(t, result)
	require.Len(t, out.Attachments, 1)
	att := out.Attachments[0]
	assert.Contains(t, att.Title, "CPU on web1 (last hour)")
	assert.Equal(t, "20.0%", fieldValue(t, att, "Now"))
}

// TestExecuteAIToolHostMetricsJobFilter checks the configured job filter reaches the PromQL for
// both the main query and the nodename fallback probe.
func TestExecuteAIToolHostMetricsJobFilter(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		expr := exprOf(t, r)
		assert.Contains(t, expr, `job="node"`)
		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true, JobFilter: "node"})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "load"}))
	require.Empty(t, result.Error)
	assert.Contains(t, result.Text, "no data")
}

// TestExecuteAIToolHostMetricsCustomInstanceLabel checks a non-default instance label setting
// reaches the PromQL (and its "avg by (...)" grouping, for cpu).
func TestExecuteAIToolHostMetricsCustomInstanceLabel(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		expr := exprOf(t, r)
		if !strings.Contains(expr, "node_uname_info") {
			// The main query, not the nodename fallback probe it triggers once this returns no
			// data below.
			assert.Contains(t, expr, `node_name=~"www-1(:[0-9]+)?"`)
			assert.Contains(t, expr, "avg by (node_name)")
		}
		resp := map[string]any{"results": map[string]any{"A": map[string]any{"frames": []any{}}}}
		encoded, _ := json.Marshal(resp)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true, InstanceLabel: "node_name"})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolHostMetrics, "u1", "c1", "", map[string]string{"host": "www-1", "metric": "cpu"}))
	require.Empty(t, result.Error)
}

// TestExecuteAIToolListHosts checks list_hosts merges and dedupes the instance and nodename
// labels' values into its "host" JSON result.
func TestExecuteAIToolListHosts(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		var out map[string]any
		switch {
		case strings.HasSuffix(r.URL.Path, "/label/instance/values"):
			out = map[string]any{"status": "success", "data": []string{"www-1", "localhost:9090"}}
		case strings.HasSuffix(r.URL.Path, "/label/nodename/values"):
			out = map[string]any{"status": "success", "data": []string{"www-1"}}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		encoded, _ := json.Marshal(out)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolListHosts, "u1", "c1", "", map[string]string{}))
	require.Empty(t, result.Error)
	// list_hosts keeps its plain [{"host":...}] shape: the assistant's dictionary reads it
	// directly, so it must never gain a "display"/"attachments" wrapper like the other tools.
	assert.True(t, strings.HasPrefix(result.Text, "["), result.Text)
	assert.NotContains(t, result.Text, "attachments")
	assert.NotContains(t, result.Text, "summary")

	var entries []struct {
		Host string `json:"host"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Text), &entries))
	var hosts []string
	for _, e := range entries {
		hosts = append(hosts, e.Host)
	}
	assert.ElementsMatch(t, []string{"www-1", "localhost:9090"}, hosts)
}

// TestExecuteAIToolListHostsJobFilter checks the configured job filter is passed as the label
// values call's match[] selector.
func TestExecuteAIToolListHostsJobFilter(t *testing.T) {
	fake := newGrafanaFake(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, `{job="node"}`, r.URL.Query().Get("match[]"))
		out := map[string]any{"status": "success", "data": []string{}}
		encoded, _ := json.Marshal(out)
		_, _ = w.Write(encoded)
	})
	env := newTestEnv(t, fake.URL, configuration{AllCommunities: true, JobFilter: "node"})
	env.allowChannel("u1", "c1", "")
	env.expectLogs(nil)

	result := env.p.ExecuteAITool(nil, toolExecution(toolListHosts, "u1", "c1", "", map[string]string{}))
	require.Empty(t, result.Error)
	assert.Equal(t, "[]", result.Text)
}
