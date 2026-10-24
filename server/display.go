// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// This app's tool results are message attachments: the same card format Antimatter's own bot
// posts use (model.MessageAttachment), so the assistant renders them exactly like a formatted
// message instead of raw tool text. See the task's ASSISTANT_DISPLAY.md for the format this
// follows.

// toolResult is the JSON envelope every converted tool (host_metrics, query_metrics, panel_data,
// search_dashboards, list_alerts) returns as its AIToolResult.Text: the attachments and a
// plain-text summary for screen readers and the assistant card's subtitle. list_hosts and
// get_dashboard are unchanged (list_hosts is read by the assistant's dictionary as a plain
// array; get_dashboard stays plain text).
type toolResult struct {
	Attachments []*model.MessageAttachment `json:"attachments,omitempty"`
	Summary     string                     `json:"summary"`
}

// encodeResult JSON-encodes a tool's attachments and summary.
func encodeResult(attachments []*model.MessageAttachment, summary string) (string, error) {
	encoded, err := json.Marshal(toolResult{Attachments: attachments, Summary: summary})
	if err != nil {
		return "", fmt.Errorf("couldn't encode the result: %w", err)
	}
	return string(encoded), nil
}

// The units this app knows how to format a value as. "" (unitCount's zero value) is a plain
// number: used for a PromQL query_metrics can't know the unit of, and for anything host_metrics
// or panel_data doesn't recognise.
const (
	unitPercent        = "percent"
	unitBytes          = "bytes"
	unitBytesPerSecond = "bytes_per_second"
	unitSeconds        = "seconds"
)

// grafanaColour is the colour of this app's metric attachments: Grafana's own brand orange.
const grafanaColour = "#F46800"

const (
	// maxChartPoints is the most values a chart.values keeps (the task's attachment format caps
	// it at 60, lower than the up-to-120 points a seriesSummary itself keeps).
	maxChartPoints = 60
	// maxShownAttachments is the most attachments a multi-item result (several series, several
	// alert rules, several dashboards) shows before a final "+N more" attachment, keeping the
	// result well under the assistant's 64 KiB cap.
	maxShownAttachments = 10
)

// formatUnitValue formats v as this app's attachment fields and text show it, by unit.
func formatUnitValue(unit string, v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "n/a"
	}
	switch unit {
	case unitPercent:
		return strconv.FormatFloat(v, 'f', 1, 64) + "%"
	case unitBytesPerSecond:
		return formatBytes(v) + "/s"
	case unitBytes:
		return formatBytes(v)
	case unitSeconds:
		return strconv.FormatFloat(v, 'f', 2, 64) + "s"
	default:
		return formatPlainNumber(v)
	}
}

// formatBytes formats v (bytes) with a binary (1024) unit prefix, one decimal place.
func formatBytes(v float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	neg := v < 0
	if neg {
		v = -v
	}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	if neg {
		s = "-" + s
	}
	return s + " " + units[i]
}

// formatPlainNumber formats v with at most 2 decimal places, trimming trailing zeroes (and the
// point itself, when there's nothing after it): the default for a value with no known unit (a
// generic query_metrics result, or load, which host_metrics reports as a plain count).
func formatPlainNumber(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// safeHTTPLink gives raw back when it's an http(s) URL, else "": every link this app's
// attachments set (TitleLink, ImageURL) goes through this, since a server's own public URL
// setting is never guaranteed to be one (an admin could set a javascript: or other scheme by
// mistake, and the attachment format forbids anything but http(s) regardless).
func safeHTTPLink(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// chartValuesFrom gives a series' points as a chart's values: finite (NaN and Inf, which a gap in
// the data can produce, would fail to JSON-encode and are dropped), oldest first, downsampled to
// at most maxChartPoints.
func chartValuesFrom(points []point) []float64 {
	finite := make([]point, 0, len(points))
	for _, p := range points {
		if !math.IsNaN(p.V) && !math.IsInf(p.V, 0) {
			finite = append(finite, p)
		}
	}
	sampled := downsample(finite, maxChartPoints)
	values := make([]float64, len(sampled))
	for i, p := range sampled {
		values[i] = p.V
	}
	return values
}

// seriesTitle names one series for an attachment's title: base on its own when the series has no
// labels of interest (labelSummary's "value" placeholder), else "base — labels" so a series is
// identifiable by what it's about (such as network's rx/tx, or a query_metrics result's own
// label set), whether it's the only series or one of several.
func seriesTitle(base string, s seriesSummary) string {
	if s.Labels == "" || s.Labels == "value" {
		return base
	}
	// Leave out the labels a reader doesn't need: the metric's own name, the scrape job, and an instance the
	// title already names (host_metrics' "CPU on www-1").
	var kept []string
	for pair := range strings.SplitSeq(s.Labels, ", ") {
		key, value, _ := strings.Cut(pair, "=")
		switch {
		case key == "__name__", key == "job":
		case key == "instance" && strings.Contains(base, strings.SplitN(value, ":", 2)[0]):
		default:
			kept = append(kept, pair)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + " — " + strings.Join(kept, ", ")
}

// metricAttachment builds one attachment for a single series: Now/Peak/Low/Average fields, a
// sparkline chart of its points, and the given title/link/footer.
func metricAttachment(title, link, footer, unit string, s seriesSummary) *model.MessageAttachment {
	now, peak, low, avg := formatUnitValue(unit, s.Last), formatUnitValue(unit, s.Max), formatUnitValue(unit, s.Min), formatUnitValue(unit, s.Avg)
	att := &model.MessageAttachment{
		Fallback:   title + ": " + now + " now, " + peak + " peak",
		Color:      grafanaColour,
		AuthorName: "Grafana",
		Title:      title,
		TitleLink:  safeHTTPLink(link),
		Text:       now + " now · peak " + peak + " · average " + avg,
		Fields: []*model.MessageAttachmentField{
			{Title: "Now", Value: now, Short: true},
			{Title: "Peak", Value: peak, Short: true},
			{Title: "Low", Value: low, Short: true},
			{Title: "Average", Value: avg, Short: true},
		},
		Footer: footer,
	}
	if values := chartValuesFrom(s.Points); len(values) >= 2 {
		label := title
		if len(label) > model.MessageAttachmentChartLabelMax {
			label = label[:model.MessageAttachmentChartLabelMax]
		}
		att.Chart = &model.MessageAttachmentChart{Values: values, Label: label}
	}
	return att
}

// metricsSummarySentence gives the plain-text summary of a metric result with exactly one
// series, such as "CPU on www-1 (last hour): 4% now, 6% peak".
func metricsSummarySentence(title string, s seriesSummary, unit string) string {
	return fmt.Sprintf("%s: %s now, %s peak", title, formatUnitValue(unit, s.Last), formatUnitValue(unit, s.Max))
}

// noDataAttachment gives the single calm attachment a metrics tool returns when its query had no
// data.
func noDataAttachment(title string) *model.MessageAttachment {
	return &model.MessageAttachment{Fallback: title + ": no data", Color: grafanaColour, AuthorName: "Grafana", Title: title, Text: "No data for this query."}
}

// buildMetricAttachments builds a metric tool's attachments and summary from its series: one
// attachment per series (up to maxShownAttachments, then a "+N more" attachment), or a single
// calm attachment when there's no data.
func buildMetricAttachments(title, link, footer, unit string, series []seriesSummary) ([]*model.MessageAttachment, string) {
	if len(series) == 0 {
		return []*model.MessageAttachment{noDataAttachment(title)}, title + ": no data"
	}
	multi := len(series) > 1
	shown := series
	var more int
	if len(shown) > maxShownAttachments {
		more = len(shown) - maxShownAttachments
		shown = shown[:maxShownAttachments]
	}
	attachments := make([]*model.MessageAttachment, 0, len(shown)+1)
	for _, s := range shown {
		attachments = append(attachments, metricAttachment(seriesTitle(title, s), link, footer, unit, s))
	}
	if more > 0 {
		attachments = append(attachments, moreAttachment(more, "series"))
	}
	summary := metricsSummarySentence(seriesTitle(title, series[0]), series[0], unit)
	if multi {
		summary = fmt.Sprintf("%s: %d series, %s now", title, len(series), formatUnitValue(unit, series[0].Last))
	}
	return attachments, summary
}

// moreAttachment gives the trailing "+N more" attachment a multi-item result adds once its own
// items exceed what it shows as full attachments.
func moreAttachment(n int, noun string) *model.MessageAttachment {
	text := fmt.Sprintf("+%d more %s", n, noun)
	return &model.MessageAttachment{Fallback: text, Text: text}
}

// markdownTable renders rows under header as a markdown table, for search_dashboards' attachment
// text.
func markdownTable(header []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat(" --- |", len(header)) + "\n")
	for _, row := range rows {
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// alertRuleColour gives one list_alerts attachment's colour from its rule state: danger while
// firing, amber while pending, and green otherwise (inactive, or anything else the ruler API
// might report).
func alertRuleColour(state string) string {
	switch strings.ToLower(state) {
	case "firing":
		return "danger"
	case "pending":
		return "warning"
	default:
		return "good"
	}
}

// alertRuleAttachment builds one list_alerts attachment for a Grafana-managed alert rule: its
// state and severity (its label, defaulting to "info" when it has none), target (its instance,
// or job), summary (its annotations) and, while it's active, when it started.
func alertRuleAttachment(r alertRuleInfo) *model.MessageAttachment {
	severity := strings.ToLower(r.Labels["severity"])
	if severity == "" {
		severity = "info"
	}
	target := r.Labels["instance"]
	if target == "" {
		target = r.Labels["job"]
	}
	text := r.Annotations["summary"]
	if text == "" {
		text = r.Annotations["description"]
	}
	att := &model.MessageAttachment{
		Fallback:   r.Name + ": " + r.State,
		Color:      alertRuleColour(r.State),
		AuthorName: "Grafana",
		Title:      r.Name,
		Text:       text,
		Footer:     "Grafana · " + r.Group,
		Fields: []*model.MessageAttachmentField{
			{Title: "State", Value: strings.ToUpper(r.State), Short: true},
			{Title: "Severity", Value: severity, Short: true},
		},
	}
	if target != "" {
		att.Fields = append(att.Fields, &model.MessageAttachmentField{Title: "Target", Value: target, Short: true})
	}
	if r.Health != "" && r.Health != "ok" {
		att.Fields = append(att.Fields, &model.MessageAttachmentField{Title: "Health", Value: r.Health, Short: true})
	}
	if since := formatActiveAt(r.ActiveAt); since != "" {
		att.Fields = append(att.Fields, &model.MessageAttachmentField{Title: "Since", Value: since, Short: true})
	}
	return att
}

// formatActiveAt formats a Prometheus rules API "activeAt" RFC 3339 timestamp the way an alert
// rule's Since field shows it, "" when it's empty (a rule that isn't currently active has none)
// or doesn't parse.
func formatActiveAt(raw string) string {
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

// grafanaUnitKind maps a subset of Grafana's own fieldConfig.defaults.unit codes to this app's
// units, and the scale to multiply a raw value by before formatting (Grafana's "percentunit"
// holds a 0..1 fraction, displayed as a percentage). Grafana has dozens of unit codes; this
// understands the common ones an ops dashboard panel is likely to use. An unrecognised or unset
// code resolves to "" (a plain number) with scale 1.
func grafanaUnitKind(unit string) (kind string, scale float64) {
	switch unit {
	case "percent":
		return unitPercent, 1
	case "percentunit":
		return unitPercent, 100
	case "bytes", "decbytes":
		return unitBytes, 1
	case "Bps", "binBps":
		return unitBytesPerSecond, 1
	case "s":
		return unitSeconds, 1
	default:
		return "", 1
	}
}

// scaleSeries multiplies every value of s (last, min, max, avg and every point) by scale,
// in place of panel_data converting a Grafana "percentunit" field (0..1) to the percentage this
// app's unitPercent formatting expects.
func scaleSeries(s seriesSummary, scale float64) seriesSummary {
	if scale == 1 {
		return s
	}
	s.Last *= scale
	s.Min *= scale
	s.Max *= scale
	s.Avg *= scale
	points := make([]point, len(s.Points))
	for i, p := range s.Points {
		points[i] = point{T: p.T, V: p.V * scale}
	}
	s.Points = points
	return s
}
