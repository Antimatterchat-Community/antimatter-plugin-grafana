// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLabelSummary(t *testing.T) {
	assert.Equal(t, "value", labelSummary(nil))
	assert.Equal(t, "value", labelSummary(map[string]string{}))
	assert.Equal(t, "instance=www-1, job=node", labelSummary(map[string]string{"job": "node", "instance": "www-1"}))
}

func TestDownsampleKeepsFirstAndLastAndCaps(t *testing.T) {
	pts := make([]point, 300)
	for i := range pts {
		pts[i] = point{T: int64(i), V: float64(i)}
	}
	out := downsample(pts, 120)
	require.Len(t, out, 120)
	assert.Equal(t, pts[0], out[0])
	assert.Equal(t, pts[len(pts)-1], out[len(out)-1])

	// Fewer points than the cap: every point is kept, unchanged.
	short := pts[:50]
	assert.Equal(t, short, downsample(short, 120))
}

func numbers(values ...float64) []json.Number {
	out := make([]json.Number, len(values))
	for i, v := range values {
		out[i] = json.Number(jsonFloat(v))
	}
	return out
}

func jsonFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSummarizeFramesStats(t *testing.T) {
	frame := dataFrame{}
	frame.Schema.Fields = []dataField{
		{Name: "Time", Type: "time"},
		{Name: "Value", Type: "number", Labels: map[string]string{"instance": "www-1"}},
	}
	frame.Data.Values = [][]json.Number{
		numbers(0, 1000, 2000, 3000),
		numbers(10, 20, 5, 15), // last=15, min=5, max=20, avg=12.5
	}
	series := summarizeFrames([]dataFrame{frame})
	require.Len(t, series, 1)
	s := series[0]
	assert.Equal(t, "instance=www-1", s.Labels)
	assert.Equal(t, 15.0, s.Last)
	assert.Equal(t, 5.0, s.Min)
	assert.Equal(t, 20.0, s.Max)
	assert.Equal(t, 12.5, s.Avg)
	require.Len(t, s.Points, 4)
}

func TestSummarizeFramesIgnoresNaN(t *testing.T) {
	frame := dataFrame{}
	frame.Schema.Fields = []dataField{{Name: "Time"}, {Name: "Value"}}
	frame.Data.Values = [][]json.Number{
		numbers(0, 1000, 2000),
		{json.Number("NaN"), json.Number("10"), json.Number("NaN")},
	}
	series := summarizeFrames([]dataFrame{frame})
	require.Len(t, series, 1)
	assert.Equal(t, 10.0, series[0].Min)
	assert.Equal(t, 10.0, series[0].Max)
	assert.Equal(t, 10.0, series[0].Avg)
	assert.Equal(t, 10.0, series[0].Last)
	assert.True(t, math.IsNaN(series[0].Points[0].V))
}

func TestExploreAndDashboardLinks(t *testing.T) {
	assert.Equal(t, "https://g.example/d/abc", dashboardLink("https://g.example", "abc", ""))
	assert.Equal(t, "https://g.example/d/abc?viewPanel=2", dashboardLink("https://g.example", "abc", "2"))
	assert.Contains(t, exploreLink("https://g.example", "prometheus", "up", "now-1h", "now"), "https://g.example/explore?left=")
}
