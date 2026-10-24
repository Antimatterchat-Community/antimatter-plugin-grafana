// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// The names this app registers its tools under (RegisterAITools adds the app's own ID as their
// namespace, so the model sees "com.antimatterchat.grafana.search_dashboards" and so on).
const (
	toolSearchDashboards = "search_dashboards"
	toolGetDashboard     = "get_dashboard"
	toolQueryMetrics     = "query_metrics"
	toolPanelData        = "panel_data"
	toolListAlerts       = "list_alerts"
	toolHostMetrics      = "host_metrics"
	toolListHosts        = "list_hosts"
)

// aiToolDefinitions declares this app's tools. All of them only read: none of them changes
// anything in Grafana, so all are kind "read" and none asks the person to confirm before running.
//
// None declares Permissions: the server's generic permission check (AIToolDefinition.Permissions)
// checks a channel or community named by one of the tool's own arguments, and none of these tools
// takes one (they always act on the conversation the call happened in, which the server instead
// gives as call.ChannelId/call.TeamId). authorizeToolCall (community.go) checks that context
// itself, in ExecuteAITool, as the brief for this app asks.
func aiToolDefinitions() []*model.AIToolDefinition {
	return []*model.AIToolDefinition{
		{
			Name:        toolSearchDashboards,
			Title:       "Search Grafana dashboards",
			Description: "Searches Grafana dashboards by title. Returns each match's title, uid, folder and URL.",
			Kind:        model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Words to search dashboard titles for. Empty lists every dashboard."}
				},
				"required": ["query"]
			}`),
		},
		{
			Name:        toolGetDashboard,
			Title:       "Get a Grafana dashboard",
			Description: "Gives one dashboard's panels, with each panel's title and the queries it runs.",
			Kind:        model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"uid": {"type": "string", "description": "The dashboard's uid, from search_dashboards."}
				},
				"required": ["uid"]
			}`),
		},
		{
			Name:  toolQueryMetrics,
			Title: "Query Grafana metrics",
			Description: "Runs a PromQL query through Grafana's own datasource query API, the same one Grafana's " +
				"dashboards and Explore use. Returns each series' labels, its last, min, max and avg values, and up " +
				"to 120 downsampled points, plus an Explore link.",
			Kind: model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"expr": {"type": "string", "description": "The PromQL expression to run."},
					"datasource_uid": {"type": "string", "description": "The Prometheus datasource's uid. Defaults to \"prometheus\"."},
					"from": {"type": "string", "description": "Start of the time range, such as \"now-1h\" or \"now-6h\". Defaults to \"now-1h\"."},
					"to": {"type": "string", "description": "End of the time range, such as \"now\". Defaults to \"now\"."},
					"step": {"type": "string", "description": "The query resolution step, in seconds, such as \"60\". Grafana picks one if omitted."}
				},
				"required": ["expr"]
			}`),
		},
		{
			Name:        toolPanelData,
			Title:       "Get a Grafana panel's data",
			Description: "Runs one dashboard panel's own queries (not a new one) and returns the same kind of result as query_metrics, plus a dashboard link.",
			Kind:        model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"dashboard_uid": {"type": "string", "description": "The dashboard's uid, from search_dashboards."},
					"panel_id": {"type": "string", "description": "The panel's id, from get_dashboard."},
					"from": {"type": "string", "description": "Start of the time range, such as \"now-1h\". Defaults to \"now-1h\"."},
					"to": {"type": "string", "description": "End of the time range, such as \"now\". Defaults to \"now\"."}
				},
				"required": ["dashboard_uid", "panel_id"]
			}`),
		},
		{
			Name:        toolListAlerts,
			Title:       "List Grafana alerts",
			Description: "Lists Grafana-managed alert rules and their current state (inactive, pending or firing), via Grafana's alerting API.",
			Kind:        model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"state": {"type": "string", "description": "Only list rules in this state: inactive, pending or firing. Omit for every rule."}
				}
			}`),
		},
		{
			Name:  toolHostMetrics,
			Title: "Host metrics",
			Description: "Gives one host's CPU, memory, disk, load or network usage, from node-exporter metrics in Grafana's " +
				"Prometheus datasource. Builds the query itself from node-exporter's own conventions: no PromQL needed. " +
				"Returns the same kind of result as query_metrics (last, min, max, avg and downsampled points).",
			Kind: model.AIToolKindRead,
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"host": {"type": "string", "description": "The host's name, such as \"www-1\": matched against Prometheus' instance label (exactly, or with a port) or node-exporter's nodename, whichever has data."},
					"metric": {"type": "string", "description": "One of cpu, memory (or mem, ram), disk, load, network (or traffic)."},
					"from": {"type": "string", "description": "Start of the time range, such as \"now-1h\" or \"now-6h\". Defaults to \"now-1h\"."},
					"to": {"type": "string", "description": "End of the time range, such as \"now\". Defaults to \"now\"."}
				},
				"required": ["host", "metric"]
			}`),
		},
		{
			Name:  toolListHosts,
			Title: "List hosts",
			Description: "Lists the distinct host names Grafana's Prometheus datasource knows, from the instance and node-exporter " +
				"nodename labels. Meant as a dictionary source for the assistant, so it learns host names; see the README.",
			Kind:        model.AIToolKindRead,
			InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		},
	}
}

// toolTimeout bounds how long handling one tool call may take in total, even one (panel_data)
// that makes more than one request to Grafana.
const toolTimeout = 10 * time.Second

// ExecuteAITool runs one of this app's tools for the person the server verified (call.UserId).
// It acts only with that person's own rights: authorizeToolCall checks they may use the tool
// where they called it from before anything reaches Grafana.
func (p *Plugin) ExecuteAITool(_ *plugin.Context, call *model.AIToolExecution) *model.AIToolResult {
	cfg := p.getConfiguration()
	if cfg.grafanaURL() == "" || cfg.ServiceAccountToken == "" {
		return &model.AIToolResult{Error: "The Grafana app isn't configured on this server yet."}
	}
	if reason := p.authorizeToolCall(cfg, call); reason != "" {
		return &model.AIToolResult{Error: reason}
	}
	if !p.toolLimiter.allow(call.UserId, time.Now()) {
		return &model.AIToolResult{Error: "Too many Grafana tool calls in a short time; wait a moment and try again."}
	}

	localName := strings.TrimPrefix(call.Tool, manifest.Id+".")

	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	client := newGrafanaClient(cfg.grafanaURL(), cfg.ServiceAccountToken)

	var (
		text string
		err  error
	)
	switch localName {
	case toolSearchDashboards:
		text, err = handleSearchDashboards(ctx, client, cfg, call.Arguments)
	case toolGetDashboard:
		text, err = handleGetDashboard(ctx, client, cfg, call.Arguments)
	case toolQueryMetrics:
		text, err = handleQueryMetrics(ctx, client, cfg, call.Arguments)
	case toolPanelData:
		text, err = handlePanelData(ctx, client, p.renderer, cfg, call.Arguments)
	case toolListAlerts:
		text, err = handleListAlerts(ctx, client, call.Arguments)
	case toolHostMetrics:
		text, err = handleHostMetrics(ctx, client, cfg, call.Arguments)
	case toolListHosts:
		text, err = handleListHosts(ctx, client, cfg)
	default:
		return &model.AIToolResult{Error: "Unknown Grafana tool."}
	}
	if err != nil {
		// err (from client.go's errGrafana, or a decoding error below) never contains the service
		// account token or the raw arguments: log and return it as it is.
		p.API.LogWarn("Grafana tool call failed", "tool", localName, "error", err.Error())
		return &model.AIToolResult{Error: "The Grafana tool failed: " + err.Error()}
	}
	return &model.AIToolResult{Text: text}
}

// decodeArguments decodes a tool call's JSON arguments into dst, defaulting to the zero value for
// an empty call.
func decodeArguments(raw json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

type searchDashboardsArgs struct {
	Query string `json:"query"`
}

func handleSearchDashboards(ctx context.Context, client *grafanaClient, cfg *configuration, raw json.RawMessage) (string, error) {
	var args searchDashboardsArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	hits, err := client.searchDashboards(ctx, args.Query)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		text := "No dashboards matched."
		att := &model.MessageAttachment{Fallback: text, AuthorName: "Grafana", Text: text}
		return encodeResult([]*model.MessageAttachment{att}, text)
	}
	rows := make([][]string, len(hits))
	for i, hit := range hits {
		folder := hit.FolderTitle
		if folder == "" {
			folder = "General"
		}
		link := hit.URL
		if link != "" {
			link = cfg.publicURL() + link
		} else {
			link = dashboardLink(cfg.publicURL(), hit.UID, "")
		}
		cell := ""
		if safe := safeHTTPLink(link); safe != "" {
			cell = "[Open](" + safe + ")"
		}
		rows[i] = []string{hit.Title, hit.UID, folder, cell}
	}
	summary := fmt.Sprintf("Found %d dashboard(s) matching %q.", len(hits), args.Query)
	attachment := &model.MessageAttachment{
		Fallback:   summary,
		AuthorName: "Grafana",
		Title:      "Grafana dashboards",
		Text:       markdownTable([]string{"Title", "UID", "Folder", "Link"}, rows),
		Footer:     "Grafana",
	}
	return encodeResult([]*model.MessageAttachment{attachment}, summary)
}

type getDashboardArgs struct {
	UID string `json:"uid"`
}

func handleGetDashboard(ctx context.Context, client *grafanaClient, cfg *configuration, raw json.RawMessage) (string, error) {
	var args getDashboardArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	if args.UID == "" {
		return "", fmt.Errorf("uid is required")
	}
	resp, err := client.getDashboard(ctx, args.UID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Dashboard: %s (uid: %s)\n", resp.Dashboard.Title, resp.Dashboard.UID)
	if len(resp.Dashboard.Panels) == 0 {
		b.WriteString("No panels.\n")
	}
	for _, panel := range resp.Dashboard.Panels {
		fmt.Fprintf(&b, "- [%s] %s (%s): %s\n", panel.ID.String(), panel.Title, panel.Type, panelQueriesSummary(panel.Targets))
	}
	b.WriteString(dashboardLink(cfg.publicURL(), resp.Dashboard.UID, ""))
	return b.String(), nil
}

// panelQueriesSummary describes a panel's targets' own queries: the "expr" (Prometheus), "query"
// or "target" field of each, whichever it has, since the field used depends on the datasource
// type.
func panelQueriesSummary(targets []map[string]any) string {
	var parts []string
	for _, target := range targets {
		for _, key := range []string{"expr", "query", "target"} {
			if q, ok := target[key].(string); ok && q != "" {
				parts = append(parts, q)
				break
			}
		}
	}
	if len(parts) == 0 {
		return "(no query)"
	}
	return strings.Join(parts, "; ")
}

type queryMetricsArgs struct {
	Expr          string `json:"expr"`
	DatasourceUID string `json:"datasource_uid"`
	From          string `json:"from"`
	To            string `json:"to"`
	Step          string `json:"step"`
}

// defaultDatasourceUID is used when a tool call doesn't name a datasource: the usual case for a
// server with one Prometheus datasource, such as the test stack this app was built against.
const defaultDatasourceUID = "prometheus"

func handleQueryMetrics(ctx context.Context, client *grafanaClient, cfg *configuration, raw json.RawMessage) (string, error) {
	var args queryMetricsArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	if strings.TrimSpace(args.Expr) == "" {
		return "", fmt.Errorf("expr is required")
	}
	datasourceUID := args.DatasourceUID
	if datasourceUID == "" {
		datasourceUID = defaultDatasourceUID
	}
	from, to := timeRangeOrDefault(args.From, args.To)

	q := dsQuery{RefID: "A", Datasource: &datasourceRef{UID: datasourceUID}, MaxDataPoints: 1000, Extra: map[string]any{"expr": args.Expr}}
	if ms := intervalMillis(args.Step); ms > 0 {
		q.IntervalMs = ms
	}
	resp, err := client.queryDS(ctx, "query_metrics", []dsQuery{q}, from, to)
	if err != nil {
		return "", err
	}
	result := resp.Results["A"]
	if result.Error != "" {
		return "", fmt.Errorf("grafana: %s", result.Error)
	}
	series := summarizeFrames(result.Frames)
	link := exploreLink(cfg.publicURL(), datasourceUID, args.Expr, from, to)
	attachments, summary := buildMetricAttachments(args.Expr, link, "Grafana · Explore", "", series)
	return encodeResult(attachments, summary)
}

type panelDataArgs struct {
	DashboardUID string `json:"dashboard_uid"`
	PanelID      string `json:"panel_id"`
	From         string `json:"from"`
	To           string `json:"to"`
}

func handlePanelData(ctx context.Context, client *grafanaClient, renderer *rendererDetector, cfg *configuration, raw json.RawMessage) (string, error) {
	var args panelDataArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	if args.DashboardUID == "" || args.PanelID == "" {
		return "", fmt.Errorf("dashboard_uid and panel_id are required")
	}
	dash, err := client.getDashboard(ctx, args.DashboardUID)
	if err != nil {
		return "", err
	}
	var panel *panelModel
	for i := range dash.Dashboard.Panels {
		if dash.Dashboard.Panels[i].ID.String() == args.PanelID {
			panel = &dash.Dashboard.Panels[i]
			break
		}
	}
	if panel == nil {
		return "", fmt.Errorf("panel %s wasn't found on dashboard %s", args.PanelID, args.DashboardUID)
	}

	from, to := timeRangeOrDefault(args.From, args.To)
	queries := panelTargetsToQueries(panel)
	if len(queries) == 0 {
		return "", fmt.Errorf("panel %s has no queries", args.PanelID)
	}
	resp, err := client.queryDS(ctx, "panel_data", queries, from, to)
	if err != nil {
		return "", err
	}

	unit, scale := grafanaUnitKind(panel.FieldConfig.Defaults.Unit)
	var series []seriesSummary
	for _, q := range queries {
		result := resp.Results[q.RefID]
		if result.Error != "" {
			return "", fmt.Errorf("grafana: %s", result.Error)
		}
		for _, s := range summarizeFrames(result.Frames) {
			series = append(series, scaleSeries(s, scale))
		}
	}
	link := dashboardLink(cfg.publicURL(), args.DashboardUID, args.PanelID)
	footer := "Grafana · " + dash.Dashboard.Title
	attachments, summary := buildMetricAttachments(panel.Title, link, footer, unit, series)
	if len(series) > 0 && len(attachments) > 0 && renderer.isInstalled(ctx, client, time.Now()) {
		if img := safeHTTPLink(renderLink(cfg.publicURL(), args.DashboardUID, args.PanelID, from, to)); img != "" {
			attachments[0].ImageURL = img
		}
	}
	return encodeResult(attachments, summary)
}

// panelTargetsToQueries turns a panel's own targets into /api/ds/query queries, falling back to
// the panel's own datasource and a lettered refId when a target doesn't set its own.
func panelTargetsToQueries(panel *panelModel) []dsQuery {
	queries := make([]dsQuery, 0, len(panel.Targets))
	for i, target := range panel.Targets {
		refID, _ := target["refId"].(string)
		if refID == "" {
			refID = fmt.Sprintf("Q%d", i)
		}
		ds := panel.Datasource
		if raw, ok := target["datasource"].(map[string]any); ok {
			if uid, ok := raw["uid"].(string); ok && uid != "" {
				ds = &datasourceRef{UID: uid}
			}
		}
		queries = append(queries, dsQuery{RefID: refID, Datasource: ds, MaxDataPoints: 1000, Extra: target})
	}
	return queries
}

type listAlertsArgs struct {
	State string `json:"state"`
}

func handleListAlerts(ctx context.Context, client *grafanaClient, raw json.RawMessage) (string, error) {
	var args listAlertsArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	rules, err := client.listAlertRules(ctx)
	if err != nil {
		return "", err
	}
	if args.State != "" {
		filtered := rules[:0:0]
		for _, r := range rules {
			if strings.EqualFold(r.State, args.State) {
				filtered = append(filtered, r)
			}
		}
		rules = filtered
	}
	if len(rules) == 0 {
		text := "No matching alert rules."
		att := &model.MessageAttachment{Fallback: text, Color: "good", AuthorName: "Grafana", Text: text}
		return encodeResult([]*model.MessageAttachment{att}, text)
	}

	firing, pending, inactive := 0, 0, 0
	for _, r := range rules {
		switch strings.ToLower(r.State) {
		case "firing":
			firing++
		case "pending":
			pending++
		default:
			inactive++
		}
	}
	shown := rules
	var more int
	if len(shown) > maxShownAttachments {
		more = len(shown) - maxShownAttachments
		shown = shown[:maxShownAttachments]
	}
	attachments := make([]*model.MessageAttachment, 0, len(shown)+1)
	for _, r := range shown {
		attachments = append(attachments, alertRuleAttachment(r))
	}
	if more > 0 {
		attachments = append(attachments, moreAttachment(more, "alert rules"))
	}
	summary := fmt.Sprintf("%d alert rule(s): %d firing, %d pending, %d inactive.", len(rules), firing, pending, inactive)
	return encodeResult(attachments, summary)
}

// timeRangeOrDefault fills in Grafana's own defaults (the last hour) when a tool call doesn't
// give a time range.
func timeRangeOrDefault(from, to string) (string, string) {
	if from == "" {
		from = "now-1h"
	}
	if to == "" {
		to = "now"
	}
	return from, to
}

// intervalMillis parses a "step" argument (seconds, as a plain number) into milliseconds; 0 if it
// doesn't parse, which leaves Grafana to pick its own interval.
func intervalMillis(step string) int {
	if step == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(step, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return int(seconds * 1000)
}
