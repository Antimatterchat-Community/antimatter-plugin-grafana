// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
)

// maxSeriesPoints is the most downsampled points a series summary keeps (query_metrics,
// panel_data).
const maxSeriesPoints = 120

// point is one downsampled point of a series: its time (Unix milliseconds) and value.
type point struct {
	T int64
	V float64
}

// seriesSummary summarizes one series (one value field of one data frame) of a /api/ds/query
// result: its labels, its last/min/max/avg values, and up to maxSeriesPoints downsampled points.
type seriesSummary struct {
	Labels string
	Last   float64
	Min    float64
	Max    float64
	Avg    float64
	Points []point
}

// labelSummary joins a series' labels into "key=value, key2=value2", sorted by key for a stable
// order; a series without labels is just called "value".
func labelSummary(labels map[string]string) string {
	if len(labels) == 0 {
		return "value"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ", ")
}

// summarizeFrames turns the frames of one /api/ds/query result into a series summary per value
// field (every field but the first, which is the time column).
func summarizeFrames(frames []dataFrame) []seriesSummary {
	var out []seriesSummary
	for _, frame := range frames {
		if len(frame.Schema.Fields) == 0 || len(frame.Data.Values) == 0 {
			continue
		}
		times := frame.Data.Values[0]
		for fieldIdx := 1; fieldIdx < len(frame.Schema.Fields) && fieldIdx < len(frame.Data.Values); fieldIdx++ {
			out = append(out, summarizeField(frame.Schema.Fields[fieldIdx], times, frame.Data.Values[fieldIdx]))
		}
	}
	return out
}

func summarizeField(field dataField, times, values []json.Number) seriesSummary {
	s := seriesSummary{Labels: labelSummary(field.Labels), Min: math.Inf(1), Max: math.Inf(-1)}
	n := min(len(times), len(values))
	var sum float64
	var count int
	all := make([]point, 0, n)
	for i := range n {
		v := numberOr(values[i], math.NaN())
		all = append(all, point{T: int64(numberOr(times[i], 0)), V: v})
		if math.IsNaN(v) {
			continue
		}
		s.Last = v
		if v < s.Min {
			s.Min = v
		}
		if v > s.Max {
			s.Max = v
		}
		sum += v
		count++
	}
	if count > 0 {
		s.Avg = sum / float64(count)
	} else {
		s.Min, s.Max = 0, 0
	}
	s.Points = downsample(all, maxSeriesPoints)
	return s
}

// downsample picks at most max points out of pts, evenly spaced, always keeping the first and
// last. It never averages or interpolates: every point it returns is one Grafana actually gave.
func downsample(pts []point, max int) []point {
	if len(pts) <= max || max <= 1 {
		return pts
	}
	out := make([]point, 0, max)
	stride := float64(len(pts)-1) / float64(max-1)
	for i := range max {
		idx := int(math.Round(float64(i) * stride))
		if idx >= len(pts) {
			idx = len(pts) - 1
		}
		out = append(out, pts[idx])
	}
	return out
}

// dashboardLink links to a dashboard, optionally with a single panel focused.
func dashboardLink(baseURL, uid, panelID string) string {
	link := baseURL + "/d/" + url.PathEscape(uid)
	if panelID != "" {
		link += "?viewPanel=" + url.QueryEscape(panelID)
	}
	return link
}

// exploreLink links to Grafana's Explore view with one query pre-filled. Grafana's own Explore
// URL format has changed between versions; this is a best-effort link using the current (left
// pane JSON) format, not guaranteed on every Grafana version.
func exploreLink(baseURL, datasourceUID, expr, from, to string) string {
	left, err := json.Marshal(map[string]any{
		"datasource": datasourceUID,
		"queries":    []map[string]any{{"refId": "A", "expr": expr, "datasource": map[string]string{"uid": datasourceUID}}},
		"range":      map[string]string{"from": from, "to": to},
	})
	if err != nil {
		return baseURL + "/explore"
	}
	return baseURL + "/explore?left=" + url.QueryEscape(string(left))
}

// renderLink links to a rendered PNG of one panel, which only works when grafana-image-renderer
// is installed (checked by the caller before this is used) and the viewer is signed in to
// Grafana.
func renderLink(baseURL, uid, panelID, from, to string) string {
	return fmt.Sprintf("%s/render/d-solo/%s/panel?panelId=%s&from=%s&to=%s&width=1000&height=500&tz=UTC",
		baseURL, url.PathEscape(uid), url.QueryEscape(panelID), url.QueryEscape(from), url.QueryEscape(to))
}
