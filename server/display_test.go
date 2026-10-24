// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatUnitValue(t *testing.T) {
	for _, tc := range []struct {
		unit string
		v    float64
		want string
	}{
		{unitPercent, 4.06, "4.1%"},
		{unitPercent, 0, "0.0%"},
		{unitBytesPerSecond, 1536, "1.5 KB/s"},
		{unitBytes, 2 * 1024 * 1024, "2.0 MB"},
		{unitSeconds, 1.5, "1.50s"},
		{"", 149, "149"},
		{"", 12.5, "12.5"},
		{"", 0, "0"},
		{"", -3.25, "-3.25"},
	} {
		t.Run(tc.unit+"_"+tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, formatUnitValue(tc.unit, tc.v))
		})
	}
	assert.Equal(t, "n/a", formatUnitValue(unitPercent, math.NaN()))
	assert.Equal(t, "n/a", formatUnitValue("", math.Inf(1)))
}

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0.0 B"},
		{512, "512.0 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{-2048, "-2.0 KB"},
	} {
		assert.Equal(t, tc.want, formatBytes(tc.v), tc.want)
	}
}

func TestGrafanaUnitKind(t *testing.T) {
	for unit, want := range map[string]string{
		"percent":  unitPercent,
		"bytes":    unitBytes,
		"decbytes": unitBytes,
		"Bps":      unitBytesPerSecond,
		"binBps":   unitBytesPerSecond,
		"s":        unitSeconds,
		"bogus":    "",
		"":         "",
	} {
		kind, scale := grafanaUnitKind(unit)
		assert.Equal(t, want, kind, unit)
		assert.Equal(t, float64(1), scale, unit)
	}
	kind, scale := grafanaUnitKind("percentunit")
	assert.Equal(t, unitPercent, kind)
	assert.Equal(t, float64(100), scale, "percentunit is a 0..1 fraction: scaled to a percentage")
}

func TestScaleSeries(t *testing.T) {
	s := seriesSummary{Last: 0.5, Min: 0.25, Max: 0.75, Avg: 0.5, Points: []point{{T: 1, V: 0.25}, {T: 2, V: 0.75}}}
	scaled := scaleSeries(s, 100)
	assert.Equal(t, 50.0, scaled.Last)
	assert.Equal(t, 25.0, scaled.Min)
	assert.Equal(t, 75.0, scaled.Max)
	assert.Equal(t, 50.0, scaled.Avg)
	assert.Equal(t, []point{{T: 1, V: 25}, {T: 2, V: 75}}, scaled.Points)

	// scale 1 is a no-op (and must not share the points slice in a way the caller must care about).
	unscaled := scaleSeries(s, 1)
	assert.Equal(t, s, unscaled)
}

// TestChartValuesFromCapsAndOrdersAndDropsNonFinite checks chart.values stays at or under
// maxChartPoints, keeps ascending time order (downsample always keeps the first and last point),
// and drops NaN/Inf values that would otherwise fail to JSON-encode.
func TestChartValuesFromCapsAndOrdersAndDropsNonFinite(t *testing.T) {
	points := make([]point, 300)
	for i := range points {
		points[i] = point{T: int64(i), V: float64(i)}
	}
	values := chartValuesFrom(points)
	require.Len(t, values, maxChartPoints)
	assert.Equal(t, 0.0, values[0])
	assert.Equal(t, 299.0, values[len(values)-1])
	for i := 1; i < len(values); i++ {
		assert.Greater(t, values[i], values[i-1], "ascending order")
	}

	withGaps := []point{{T: 0, V: 1}, {T: 1, V: math.NaN()}, {T: 2, V: math.Inf(1)}, {T: 3, V: math.Inf(-1)}, {T: 4, V: 2}}
	gapValues := chartValuesFrom(withGaps)
	assert.Equal(t, []float64{1, 2}, gapValues)

	// Must always be safe to put straight into a JSON document (NaN/Inf would error).
	_, err := json.Marshal(gapValues)
	require.NoError(t, err)

	assert.Empty(t, chartValuesFrom(nil))
	// A single point is returned as-is; metricAttachment is what decides a chart needs at least
	// two (model.MessageAttachmentChart itself requires 2-200 values).
	assert.Equal(t, []float64{1}, chartValuesFrom([]point{{T: 0, V: 1}}))
}

func TestSafeHTTPLink(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"https://grafana.example/d/abc", "https://grafana.example/d/abc"},
		{"http://127.0.0.1:3000/d/abc", "http://127.0.0.1:3000/d/abc"},
		{"javascript:alert(1)", ""},
		{"ftp://files.example/x", ""},
		{"", ""},
		{"not a url at all", ""},
		{"//grafana.example/d/abc", ""},
	} {
		assert.Equal(t, tc.want, safeHTTPLink(tc.raw), tc.raw)
	}
}

func TestMarkdownTable(t *testing.T) {
	got := markdownTable([]string{"A", "B"}, [][]string{{"1", "2"}, {"3", "4"}})
	assert.Equal(t, "| A | B |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |", got)
}

func TestFormatActiveAt(t *testing.T) {
	assert.Equal(t, "", formatActiveAt(""))
	assert.Equal(t, "", formatActiveAt("not a time"))
	assert.Equal(t, "2026-10-24 17:55 UTC", formatActiveAt("2026-10-24T17:55:00Z"))
}

func TestAlertRuleColour(t *testing.T) {
	assert.Equal(t, "danger", alertRuleColour("firing"))
	assert.Equal(t, "danger", alertRuleColour("FIRING"))
	assert.Equal(t, "warning", alertRuleColour("pending"))
	assert.Equal(t, "good", alertRuleColour("inactive"))
	assert.Equal(t, "good", alertRuleColour(""))
}

// TestAlertRuleAttachmentSeverityDefault checks a rule without a severity label still gets one
// ("info"), never left blank.
func TestAlertRuleAttachmentSeverityDefault(t *testing.T) {
	att := alertRuleAttachment(alertRuleInfo{Name: "Watchdog", State: "firing", Group: "default"})
	assert.Equal(t, "info", fieldValue(t, att, "Severity"))
	assert.Equal(t, "danger", att.Color)
}

// TestEncodeResultSizeIsBounded checks a result with far more series than maxShownAttachments
// (each with a full chart) still stays comfortably under the assistant's 64 KiB cap, because it's
// capped at maxShownAttachments attachments plus one "+N more" trailer.
func TestEncodeResultSizeIsBounded(t *testing.T) {
	var series []seriesSummary
	for i := range 200 {
		pts := make([]point, 200)
		for j := range pts {
			pts[j] = point{T: int64(j * 1000), V: float64(i*1000 + j)}
		}
		series = append(series, seriesSummary{Labels: "instance=host-" + strconv.Itoa(i), Last: 1, Min: 0, Max: 2, Avg: 1, Points: pts})
	}
	attachments, summary := buildMetricAttachments("many series", "https://grafana.example/explore", "Grafana", unitPercent, series)
	require.Len(t, attachments, maxShownAttachments+1)
	encoded, err := encodeResult(attachments, summary)
	require.NoError(t, err)
	assert.Less(t, len(encoded), 64*1024)
}

func TestSeriesTitleDropsLabelsTheReaderDoesntNeed(t *testing.T) {
	cases := []struct{ base, labels, want string }{
		{"Load on www-1 (last hour)", "__name__=node_load1, instance=www-1, job=node", "Load on www-1 (last hour)"},
		{"CPU on www-1 (last hour)", "instance=www-1:9100", "CPU on www-1 (last hour)"},
		{"Network on www-1 (last hour)", "device=eth0, direction=rx, instance=www-1, job=node", "Network on www-1 (last hour) — device=eth0, direction=rx"},
		{"up", "instance=api-2:9100, job=node", "up — instance=api-2:9100"},
		{"Load", "value", "Load"},
	}
	for _, c := range cases {
		if got := seriesTitle(c.base, seriesSummary{Labels: c.labels}); got != c.want {
			t.Errorf("seriesTitle(%q, %q) = %q, want %q", c.base, c.labels, got, c.want)
		}
	}
}
