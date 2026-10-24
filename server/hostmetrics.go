// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The metrics host_metrics understands, and the words a tool call's "metric" argument may use for
// each (matching case-insensitively): the assistant's rules engine fills this argument with
// whichever of these words matched its own "cpu|memory|disk|load|latency|traffic on {host}"
// pattern, so every alias that pattern recognises must resolve here too.
const (
	metricCPU     = "cpu"
	metricMemory  = "memory"
	metricDisk    = "disk"
	metricLoad    = "load"
	metricNetwork = "network"
)

// metricAliases maps every word a tool call's "metric" argument may use, lower-cased, to the
// canonical metric above.
var metricAliases = map[string]string{
	"cpu":     metricCPU,
	"memory":  metricMemory,
	"mem":     metricMemory,
	"ram":     metricMemory,
	"disk":    metricDisk,
	"load":    metricLoad,
	"network": metricNetwork,
	"traffic": metricNetwork,
}

// metricTitles names each metric the way a tool result's title shows it.
var metricTitles = map[string]string{
	metricCPU:     "CPU",
	metricMemory:  "Memory",
	metricDisk:    "Disk",
	metricLoad:    "Load",
	metricNetwork: "Network",
}

// normalizeMetric resolves a tool call's "metric" argument to one of the canonical metrics above,
// or an error naming the word that didn't match if it doesn't.
func normalizeMetric(metric string) (string, error) {
	canonical, ok := metricAliases[strings.ToLower(strings.TrimSpace(metric))]
	if !ok {
		return "", fmt.Errorf("metric %q isn't one of cpu, memory, disk, load or network (or their aliases)", metric)
	}
	return canonical, nil
}

// escapeLabelValueForPromQL makes a value safe to use as the content of a PromQL label matcher's
// string literal, whether matched with "=" or, as host_metrics and list_hosts' job filter do,
// "=~". It must defeat two different things a crafted value could otherwise do:
//
//   - break out of the string literal itself (a '"' ends it early; a trailing '\' escapes the
//     closing quote instead of ending the string), letting the rest of the value run as arbitrary
//     PromQL rather than a label's value;
//   - widen a regex match (such as "=~") far past the one host or job intended, through regex
//     metacharacters ("." matching any character, "|" alternating, "(" grouping, and so on).
//
// regexp.QuoteMeta handles the second: it escapes every RE2 metacharacter, including a literal
// backslash in the input (turning it into "\\", a regex matching one literal backslash). Its
// output, like the original value, still has to survive being written into a double-quoted PromQL
// string literal, whose escaping is the same as Go's: every backslash it now contains (whether
// from the input or added by QuoteMeta) must be doubled, and every '"' escaped, so the PromQL
// lexer reconstructs exactly the regex QuoteMeta built, nothing shorter and nothing more.
//
// See the manual check in the task report for this confirmed against a real Prometheus: the
// unescaped form of a value containing '"' breaks the query with a parse error (proving the
// injection), the escaped form here runs as an ordinary, if non-matching, query.
func escapeLabelValueForPromQL(value string) string {
	escaped := regexp.QuoteMeta(value)
	escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return escaped
}

// hostInstanceMatcher returns the PromQL matcher fragment (without braces) that matches host
// against instanceLabel exactly, or with a ":<port>" suffix (node-exporter's instance label is
// usually "host:9100", but an admin's Grafana may have relabelled it to the bare host).
func hostInstanceMatcher(instanceLabel, host string) string {
	return fmt.Sprintf(`%s=~"%s(:[0-9]+)?"`, instanceLabel, escapeLabelValueForPromQL(host))
}

// exactInstanceMatcher returns a PromQL matcher fragment that matches instanceLabel against
// exactly one of instances (used once a host has been resolved to its instance label value(s) via
// nodename).
func exactInstanceMatcher(instanceLabel string, instances []string) string {
	escaped := make([]string, len(instances))
	for i, instance := range instances {
		escaped[i] = escapeLabelValueForPromQL(instance)
	}
	return fmt.Sprintf(`%s=~"^(%s)$"`, instanceLabel, strings.Join(escaped, "|"))
}

// jobMatcher returns the PromQL matcher fragment that restricts to jobFilter, or "" when jobFilter
// is blank (match any job).
func jobMatcher(jobFilter string) string {
	if jobFilter == "" {
		return ""
	}
	return fmt.Sprintf(`job="%s"`, escapeLabelValueForPromQL(jobFilter))
}

// selector joins non-empty matcher fragments into a PromQL "{...}" selector.
func selector(matchers ...string) string {
	var kept []string
	for _, m := range matchers {
		if m != "" {
			kept = append(kept, m)
		}
	}
	return "{" + strings.Join(kept, ",") + "}"
}

// buildMetricExpr builds the PromQL expression for one of the metrics above, matching hostMatcher
// (a matcher fragment naming the host, built by hostInstanceMatcher or exactInstanceMatcher) and,
// if set, jobFilterMatcher.
func buildMetricExpr(metric, instanceLabel, hostMatcher, jobFilterMatcher string) string {
	matchers := selector(hostMatcher, jobFilterMatcher)
	switch metric {
	case metricCPU:
		return fmt.Sprintf(
			`100 * (1 - avg by (%s) (rate(node_cpu_seconds_total%s[2m])))`,
			instanceLabel, selector(`mode="idle"`, hostMatcher, jobFilterMatcher),
		)
	case metricMemory:
		return fmt.Sprintf(
			`100 * (1 - node_memory_MemAvailable_bytes%s / node_memory_MemTotal_bytes%s)`,
			matchers, matchers,
		)
	case metricDisk:
		return fmt.Sprintf(
			`100 * (1 - node_filesystem_avail_bytes%[1]s / node_filesystem_size_bytes%[1]s)`,
			selector(`mountpoint="/"`, `fstype!="rootfs"`, hostMatcher, jobFilterMatcher),
		)
	case metricLoad:
		return fmt.Sprintf(`node_load1%s`, matchers)
	case metricNetwork:
		netSelector := selector(`device!="lo"`, hostMatcher, jobFilterMatcher)
		return fmt.Sprintf(
			`sum by (direction) (label_replace(rate(node_network_receive_bytes_total%[1]s[2m]), "direction", "rx", "", "") or label_replace(rate(node_network_transmit_bytes_total%[1]s[2m]), "direction", "tx", "", ""))`,
			netSelector,
		)
	default:
		// normalizeMetric is the only caller that produces a metric value, so this never happens
		// in practice; an empty expression fails Grafana's own parser rather than silently
		// querying nothing.
		return ""
	}
}

// windowDescription describes a time range the way a host_metrics title shows it: "last hour" for
// the common "now-1h".."now" case and other short "now-<N><unit>" ranges ending "now", else the
// range as given.
var relativeWindow = regexp.MustCompile(`^now-([0-9]+)([smhdwy])$`)

func windowDescription(from, to string) string {
	if to != "now" {
		return from + " to " + to
	}
	m := relativeWindow.FindStringSubmatch(from)
	if m == nil {
		return from + " to " + to
	}
	unit := map[string]string{"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "y": "year"}[m[2]]
	if m[1] == "1" {
		return "last " + unit
	}
	return fmt.Sprintf("last %s %ss", m[1], unit)
}

// hostMetricTitle builds host_metrics' human-readable title, such as "CPU on www-1 (last hour)".
func hostMetricTitle(metric, host, from, to string) string {
	return fmt.Sprintf("%s on %s (%s)", metricTitles[metric], host, windowDescription(from, to))
}

type hostMetricsArgs struct {
	Host   string `json:"host"`
	Metric string `json:"metric"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// handleHostMetrics runs host_metrics: it builds the PromQL for args.Metric itself (from
// node-exporter conventions), so the assistant's rules engine can fill this tool's arguments
// straight from a "cpu|memory|disk|load|latency|traffic on {host}" match without ever
// constructing PromQL of its own.
func handleHostMetrics(ctx context.Context, client *grafanaClient, cfg *configuration, raw json.RawMessage) (string, error) {
	var args hostMetricsArgs
	if err := decodeArguments(raw, &args); err != nil {
		return "", fmt.Errorf("couldn't read the arguments: %w", err)
	}
	if strings.TrimSpace(args.Host) == "" {
		return "", fmt.Errorf("host is required")
	}
	metric, err := normalizeMetric(args.Metric)
	if err != nil {
		return "", err
	}
	from, to := timeRangeOrDefault(args.From, args.To)
	instanceLabel := cfg.instanceLabel()
	jf := jobMatcher(cfg.jobFilter())

	series, err := runMetricQuery(ctx, client, buildMetricExpr(metric, instanceLabel, hostInstanceMatcher(instanceLabel, args.Host), jf), from, to)
	if err != nil {
		return "", err
	}

	if len(series) == 0 {
		// The instance label (exactly, or with a port) found nothing: try resolving the host via
		// node-exporter's nodename instead, whichever one finds series.
		if instances, resolveErr := resolveInstancesByNodename(ctx, client, instanceLabel, jf, args.Host, from, to); resolveErr == nil && len(instances) > 0 {
			series, err = runMetricQuery(ctx, client, buildMetricExpr(metric, instanceLabel, exactInstanceMatcher(instanceLabel, instances), jf), from, to)
			if err != nil {
				return "", err
			}
		}
	}

	return formatSeries(hostMetricTitle(metric, args.Host, from, to), series), nil
}

// runMetricQuery runs one PromQL expression through Grafana's datasource query API and summarizes
// it, the same way query_metrics does.
func runMetricQuery(ctx context.Context, client *grafanaClient, expr, from, to string) ([]seriesSummary, error) {
	q := dsQuery{RefID: "A", Datasource: &datasourceRef{UID: defaultDatasourceUID}, MaxDataPoints: 1000, Extra: map[string]any{"expr": expr}}
	resp, err := client.queryDS(ctx, "host_metrics", []dsQuery{q}, from, to)
	if err != nil {
		return nil, err
	}
	result := resp.Results["A"]
	if result.Error != "" {
		return nil, fmt.Errorf("grafana: %s", result.Error)
	}
	return summarizeFrames(result.Frames), nil
}

// resolveInstancesByNodename finds the instanceLabel values of the series whose node_uname_info
// "nodename" label matches host, for host_metrics' fallback when host doesn't directly match the
// instance label. A query error here is swallowed (nil, nil): it only means the fallback found
// nothing, not that the tool call itself failed.
func resolveInstancesByNodename(ctx context.Context, client *grafanaClient, instanceLabel, jobFilterMatcher, host, from, to string) ([]string, error) {
	matchers := selector(fmt.Sprintf(`nodename=~"%s"`, escapeLabelValueForPromQL(host)), jobFilterMatcher)
	q := dsQuery{RefID: "A", Datasource: &datasourceRef{UID: defaultDatasourceUID}, MaxDataPoints: 10, Extra: map[string]any{"expr": "node_uname_info" + matchers}}
	resp, err := client.queryDS(ctx, "host_metrics_resolve", []dsQuery{q}, from, to)
	if err != nil {
		return nil, nil
	}
	result := resp.Results["A"]
	if result.Error != "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var instances []string
	for _, frame := range result.Frames {
		for _, field := range frame.Schema.Fields {
			if v, ok := field.Labels[instanceLabel]; ok && v != "" && !seen[v] {
				seen[v] = true
				instances = append(instances, v)
			}
		}
	}
	return instances, nil
}

// hostDictionaryLabels are the labels list_hosts collects values from: the configured instance
// label (node-exporter's own target address) and node-exporter's "nodename" (the host's own
// hostname, which can differ from how it's scraped).
func hostDictionaryLabels(instanceLabel string) []string {
	if instanceLabel == "nodename" {
		return []string{"nodename"}
	}
	return []string{instanceLabel, "nodename"}
}

// handleListHosts runs list_hosts: it lists the distinct values of the instance and nodename
// labels, through Grafana's datasource resource call, so the assistant's dictionary (a
// DictionarySources entry naming this tool and its "host" field; see the README) learns host
// names without an admin maintaining a list by hand.
func handleListHosts(ctx context.Context, client *grafanaClient, cfg *configuration) (string, error) {
	match := ""
	if jf := jobMatcher(cfg.jobFilter()); jf != "" {
		match = selector(jf)
	}

	seen := map[string]bool{}
	var hosts []string
	for _, label := range hostDictionaryLabels(cfg.instanceLabel()) {
		values, err := client.labelValues(ctx, defaultDatasourceUID, label, match)
		if err != nil {
			return "", err
		}
		for _, v := range values {
			if v != "" && !seen[v] {
				seen[v] = true
				hosts = append(hosts, v)
			}
		}
	}
	sort.Strings(hosts)

	type hostEntry struct {
		Host string `json:"host"`
	}
	entries := make([]hostEntry, len(hosts))
	for i, h := range hosts {
		entries[i] = hostEntry{Host: h}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("couldn't encode the host list: %w", err)
	}
	return string(encoded), nil
}
