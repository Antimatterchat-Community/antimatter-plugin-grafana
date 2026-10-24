// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pkg/errors"
)

const (
	// requestTimeout bounds the whole of every request to Grafana (dial, headers and body).
	requestTimeout = 10 * time.Second
	// maxResponseSize caps how much of a Grafana response this app reads. Grafana's own JSON
	// responses (dashboards, query results, alert rules) are normally well under this; a server
	// that sent much more is refused rather than read in full.
	maxResponseSize = 5 << 20 // 5 MiB
)

// limitedTransport bounds the size of Grafana's responses.
type limitedTransport struct {
	base http.RoundTripper
}

func (t limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(resp.Body, maxResponseSize), Closer: resp.Body}
	return resp, nil
}

// grafanaClient calls the Grafana HTTP API with the app's service account token. Never log or
// return the token: every error path below strips it from Grafana's own error bodies by only
// ever surfacing a short, fixed message, never the request or response verbatim.
type grafanaClient struct {
	http    *http.Client
	baseURL string
	token   string
}

func newGrafanaClient(baseURL, token string) *grafanaClient {
	transport := &http.Transport{
		DialContext:           newDialer().DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		Proxy:                 nil,
	}
	return &grafanaClient{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Transport: limitedTransport{base: transport},
			Timeout:   requestTimeout,
		},
	}
}

// errGrafana is returned for anything that goes wrong talking to Grafana. Its message never
// contains the service account token, the raw request or Grafana's raw response body (which
// could itself echo back request data); it names only the operation and the HTTP status, if any.
type errGrafana struct {
	op     string
	status int
	cause  error
}

func (e *errGrafana) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("grafana: %s failed (status %d)", e.op, e.status)
	}
	return fmt.Sprintf("grafana: %s failed", e.op)
}

func (e *errGrafana) Unwrap() error { return e.cause }

// do sends a request to Grafana and decodes its JSON body into out (if non-nil). op names the
// call, for errGrafana; it must never include the token or any request argument that might.
func (c *grafanaClient) do(ctx context.Context, op, method, path string, query url.Values, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return &errGrafana{op: op, cause: err}
		}
		bodyReader = bytes.NewReader(encoded)
	}

	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return &errGrafana{op: op, cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// http errors can embed the request URL, which never contains the token (it's a header),
		// but strip it down anyway to a fixed message to stay safe across Go stdlib versions.
		return &errGrafana{op: op, cause: errors.New("request failed")}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return &errGrafana{op: op, status: resp.StatusCode, cause: errors.New("failed to read the response")}
	}
	if resp.StatusCode >= 300 {
		return &errGrafana{op: op, status: resp.StatusCode, cause: errors.New("grafana refused the request")}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &errGrafana{op: op, status: resp.StatusCode, cause: errors.New("couldn't read grafana's response")}
	}
	return nil
}

// dashboardSearchHit is one result of GET /api/search.
type dashboardSearchHit struct {
	UID         string `json:"uid"`
	Title       string `json:"title"`
	URI         string `json:"uri"`
	Type        string `json:"type"`
	FolderTitle string `json:"folderTitle"`
	URL         string `json:"url"`
}

// searchDashboards runs Grafana's dashboard search.
func (c *grafanaClient) searchDashboards(ctx context.Context, query string) ([]dashboardSearchHit, error) {
	q := url.Values{"type": {"dash-db"}, "limit": {"20"}}
	if query != "" {
		q.Set("query", query)
	}
	var hits []dashboardSearchHit
	if err := c.do(ctx, "search_dashboards", http.MethodGet, "/api/search", q, nil, &hits); err != nil {
		return nil, err
	}
	return hits, nil
}

// datasourceRef names a datasource (by UID, the modern form) in a panel target or a /api/ds/query
// query.
type datasourceRef struct {
	UID  string `json:"uid,omitempty"`
	Type string `json:"type,omitempty"`
}

// panelModel is one panel of a dashboard, as GET /api/dashboards/uid/:uid gives it. Targets are
// kept as raw JSON objects: their shape depends on the datasource type (Prometheus panels have
// "expr"; others have "query", "target" or their own fields), and panel_data passes them through
// to /api/ds/query unchanged.
type panelModel struct {
	ID         json.Number      `json:"id"`
	Title      string           `json:"title"`
	Type       string           `json:"type"`
	Datasource *datasourceRef   `json:"datasource"`
	Targets    []map[string]any `json:"targets"`
}

// dashboardModel is a dashboard's own content, as GET /api/dashboards/uid/:uid gives it.
type dashboardModel struct {
	UID    string       `json:"uid"`
	Title  string       `json:"title"`
	Panels []panelModel `json:"panels"`
}

type dashboardGetResponse struct {
	Dashboard dashboardModel `json:"dashboard"`
	Meta      struct {
		Slug      string `json:"slug"`
		FolderURL string `json:"folderUrl"`
	} `json:"meta"`
}

// getDashboard fetches a dashboard's own definition (its panels, with their queries).
func (c *grafanaClient) getDashboard(ctx context.Context, uid string) (*dashboardGetResponse, error) {
	var out dashboardGetResponse
	if err := c.do(ctx, "get_dashboard", http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(uid), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// dsQuery is one query of a /api/ds/query request. Extra holds whatever other fields the panel's
// own target had (such as "expr" for a Prometheus query, or another datasource's own query
// fields); they're merged into the JSON object sent to Grafana.
type dsQuery struct {
	RefID         string         `json:"refId"`
	Datasource    *datasourceRef `json:"datasource,omitempty"`
	IntervalMs    int            `json:"intervalMs,omitempty"`
	MaxDataPoints int            `json:"maxDataPoints,omitempty"`
	Extra         map[string]any `json:"-"`
}

func (q dsQuery) MarshalJSON() ([]byte, error) {
	object := map[string]any{}
	maps.Copy(object, q.Extra)
	object["refId"] = q.RefID
	if q.Datasource != nil {
		object["datasource"] = q.Datasource
	}
	if q.IntervalMs != 0 {
		object["intervalMs"] = q.IntervalMs
	}
	if q.MaxDataPoints != 0 {
		object["maxDataPoints"] = q.MaxDataPoints
	}
	return json.Marshal(object)
}

type dsQueryRequest struct {
	Queries []dsQuery `json:"queries"`
	From    string    `json:"from"`
	To      string    `json:"to"`
}

// dataField is one field (column) of a data frame: a time column, or a value column with the
// series' labels.
type dataField struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
}

type dataFrame struct {
	Schema struct {
		Fields []dataField `json:"fields"`
	} `json:"schema"`
	Data struct {
		Values [][]json.Number `json:"values"`
	} `json:"data"`
}

type dsQueryResult struct {
	Frames []dataFrame `json:"frames"`
	Error  string      `json:"error"`
}

type dsQueryResponse struct {
	Results map[string]dsQueryResult `json:"results"`
}

// queryDS runs one or more queries through Grafana's datasource query API (/api/ds/query), the
// same call Grafana's own dashboards and Explore use.
func (c *grafanaClient) queryDS(ctx context.Context, op string, queries []dsQuery, from, to string) (*dsQueryResponse, error) {
	if from == "" {
		from = "now-1h"
	}
	if to == "" {
		to = "now"
	}
	var out dsQueryResponse
	req := dsQueryRequest{Queries: queries, From: from, To: to}
	if err := c.do(ctx, op, http.MethodPost, "/api/ds/query", nil, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// alertRuleInfo is one Grafana-managed alert rule, flattened from the ruler API's groups and
// files, with its current state.
type alertRuleInfo struct {
	Name   string
	Group  string
	State  string
	Health string
	Labels map[string]string
}

type rulerAlert struct {
	Labels map[string]string `json:"labels"`
	State  string            `json:"state"`
}

type rulerRule struct {
	Name   string       `json:"name"`
	State  string       `json:"state"`
	Health string       `json:"health"`
	Alerts []rulerAlert `json:"alerts"`
}

type rulerGroup struct {
	Name  string      `json:"name"`
	Rules []rulerRule `json:"rules"`
}

type rulerResponse struct {
	Data struct {
		Groups []rulerGroup `json:"groups"`
	} `json:"data"`
}

// listAlertRules lists every Grafana-managed alert rule and its state, via Grafana's
// Prometheus-compatible alerting API.
func (c *grafanaClient) listAlertRules(ctx context.Context) ([]alertRuleInfo, error) {
	var resp rulerResponse
	if err := c.do(ctx, "list_alerts", http.MethodGet, "/api/prometheus/grafana/api/v1/rules", nil, nil, &resp); err != nil {
		return nil, err
	}
	var rules []alertRuleInfo
	for _, group := range resp.Data.Groups {
		for _, rule := range group.Rules {
			info := alertRuleInfo{Name: rule.Name, Group: group.Name, State: rule.State, Health: rule.Health}
			if len(rule.Alerts) > 0 {
				info.Labels = rule.Alerts[0].Labels
			}
			rules = append(rules, info)
		}
	}
	return rules, nil
}

// pluginListItem is one plugin of GET /api/plugins.
type pluginListItem struct {
	ID string `json:"id"`
}

// hasImageRenderer tells whether the grafana-image-renderer plugin is installed, by listing
// Grafana's plugins. A failure to list them (for instance, because the service account can't)
// counts as not installed: rendering a panel image is only ever attempted, never required.
func (c *grafanaClient) hasImageRenderer(ctx context.Context) bool {
	var plugins []pluginListItem
	if err := c.do(ctx, "list_plugins", http.MethodGet, "/api/plugins", url.Values{"embedded": {"0"}}, nil, &plugins); err != nil {
		return false
	}
	for _, p := range plugins {
		if p.ID == imageRendererPluginID {
			return true
		}
	}
	return false
}

const imageRendererPluginID = "grafana-image-renderer"

// labelValuesResponse is Prometheus' own shape for GET .../api/v1/label/:name/values, which
// Grafana's datasource resource call (the same one Explore's own metric picker uses) passes
// through unchanged.
type labelValuesResponse struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// labelValues lists the values a label takes, through Grafana's datasource resource call
// (/api/datasources/uid/:uid/resources/...), the same route Explore's own label/metric picker
// uses. match, when non-empty, is a PromQL selector (such as `{job="node"}`) that restricts which
// series' labels are counted; empty considers every series.
func (c *grafanaClient) labelValues(ctx context.Context, datasourceUID, label, match string) ([]string, error) {
	var query url.Values
	if match != "" {
		query = url.Values{"match[]": {match}}
	}
	path := "/api/datasources/uid/" + url.PathEscape(datasourceUID) + "/resources/api/v1/label/" + url.PathEscape(label) + "/values"
	var out labelValuesResponse
	if err := c.do(ctx, "list_hosts", http.MethodGet, path, query, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// numberOr returns n as a float64, or fallback if n is empty or not a number.
func numberOr(n json.Number, fallback float64) float64 {
	if n == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil {
		return fallback
	}
	return f
}
