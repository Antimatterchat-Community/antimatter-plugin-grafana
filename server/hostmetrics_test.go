// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodePromQLStringLiteral mimics how PromQL (like Go) reads a double-quoted string literal:
// '\\' escapes exactly the next character, and the first unescaped '"' ends it. It returns the
// literal's decoded content and whatever text came after its closing quote, so a test can tell
// whether a crafted value's quote ended the literal somewhere other than the real end.
func decodePromQLStringLiteral(t *testing.T, s string) (content, rest string) {
	t.Helper()
	require.True(t, strings.HasPrefix(s, `"`), "not a string literal: %q", s)
	var b strings.Builder
	i := 1
	for ; i < len(s); i++ {
		switch s[i] {
		case '\\':
			require.Less(t, i+1, len(s), "orphaned backslash at %d in %q", i, s)
			b.WriteByte(s[i+1])
			i++
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	t.Fatalf("string literal in %q never closes", s)
	return "", ""
}

func TestNormalizeMetric(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"cpu", metricCPU},
		{"CPU", metricCPU},
		{" cpu ", metricCPU},
		{"memory", metricMemory},
		{"mem", metricMemory},
		{"ram", metricMemory},
		{"RAM", metricMemory},
		{"disk", metricDisk},
		{"load", metricLoad},
		{"network", metricNetwork},
		{"traffic", metricNetwork},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := normalizeMetric(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeMetricUnknown(t *testing.T) {
	_, err := normalizeMetric("bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")
}

// TestEscapeLabelValueForPromQLInjection checks that nothing in a crafted host value can escape
// the PromQL string literal it ends up in (built, below, exactly the way hostInstanceMatcher
// does) to run as PromQL rather than as the literal's content, that the literal decodes back to
// exactly the intended regex (never less, from an early unescaped quote, nor more, from stray
// text after it), and that the regex it decodes to only matches the host asked for, however many
// regex metacharacters the attempt threw at it.
func TestEscapeLabelValueForPromQLInjection(t *testing.T) {
	for name, host := range map[string]string{
		"double quote ends the string early":      `www-1" or vector(1) or (1==bool 1`,
		"trailing backslash eats the close quote": `www-1\`,
		"backslash and quote together":            `foo\"} or up{job="x"} or (1==bool 1`,
		"brace tries to close the selector":       `www-1"} or vector(1) or up{instance="x`,
		"regex alternation and wildcard":          `www-1.*|evil`,
		"regex anchors, braces and groups":        `^(a|b){2,3}$`,
		"newline":                                 "www-1\nor vector(1)",
		"only a quote":                            `"`,
		"only a backslash":                        `\`,
	} {
		t.Run(name, func(t *testing.T) {
			escaped := escapeLabelValueForPromQL(host)
			built := `instance=~"` + escaped + `(:[0-9]+)?"`

			// The literal must decode to exactly the regex QuoteMeta built for host plus the
			// fixed port suffix, and nothing must follow its closing quote: either would mean a
			// quote in host ended the literal somewhere other than the true end.
			content, rest := decodePromQLStringLiteral(t, built[len(`instance=~`):])
			assert.Equal(t, regexp.QuoteMeta(host)+"(:[0-9]+)?", content)
			assert.Equal(t, "", rest)

			// The decoded content, compiled as the regex Prometheus would run, must match host
			// itself (and with a port appended) but nothing broader: replacing one character of
			// host must stop it matching, which a live (unescaped) metacharacter wouldn't.
			re, err := regexp.Compile("^" + content + "$")
			require.NoError(t, err)
			assert.True(t, re.MatchString(host), "regex %q must match the literal host %q", re, host)
			assert.True(t, re.MatchString(host+":9100"), "regex %q must match host with a port", re)
			assert.False(t, re.MatchString(host+host), "regex %q must not match more than the one host", re)
		})
	}
}

// TestEscapeLabelValueForPromQLPlain checks a host name with no regex metacharacter or quote is
// left untouched (no spurious escaping where none is needed), and one with a metacharacter that
// has no special meaning for a person naming a host (a literal '.' in a hostname) is still
// escaped, matching plain regexp.QuoteMeta: that escaping is needed so "." only matches a literal
// dot, not "any character", which would quietly widen the match past the one host asked for.
func TestEscapeLabelValueForPromQLPlain(t *testing.T) {
	for _, host := range []string{"www-1", "www_1", "host01"} {
		assert.Equal(t, host, escapeLabelValueForPromQL(host), host)
	}
	for _, host := range []string{"web-01.internal", "10.0.0.5"} {
		// The backslash QuoteMeta adds to escape "." for the regex must itself be doubled to
		// survive the PromQL string literal it's embedded in (see escapeLabelValueForPromQL's
		// own comment).
		assert.Equal(t, strings.ReplaceAll(regexp.QuoteMeta(host), `\`, `\\`), escapeLabelValueForPromQL(host), host)
	}
}

func TestBuildMetricExprPerMetric(t *testing.T) {
	hostMatcher := hostInstanceMatcher("instance", "www-1")
	for metric, want := range map[string][]string{
		metricCPU:     {"node_cpu_seconds_total", `mode="idle"`, hostMatcher, "avg by (instance)", "rate("},
		metricMemory:  {"node_memory_MemAvailable_bytes", "node_memory_MemTotal_bytes", hostMatcher},
		metricDisk:    {"node_filesystem_avail_bytes", "node_filesystem_size_bytes", `mountpoint="/"`, `fstype!="rootfs"`, hostMatcher},
		metricLoad:    {"node_load1", hostMatcher},
		metricNetwork: {"node_network_receive_bytes_total", "node_network_transmit_bytes_total", `device!="lo"`, hostMatcher, `"direction"`},
	} {
		t.Run(metric, func(t *testing.T) {
			expr := buildMetricExpr(metric, "instance", hostMatcher, "")
			for _, substr := range want {
				assert.Contains(t, expr, substr)
			}
		})
	}
}

func TestBuildMetricExprJobFilter(t *testing.T) {
	hostMatcher := hostInstanceMatcher("instance", "www-1")
	for metric := range metricTitles {
		expr := buildMetricExpr(metric, "instance", hostMatcher, `job="node"`)
		assert.Contains(t, expr, `job="node"`, metric)
	}
}

func TestHostInstanceMatcher(t *testing.T) {
	assert.Equal(t, `instance=~"www-1(:[0-9]+)?"`, hostInstanceMatcher("instance", "www-1"))
	assert.Equal(t, `node_name=~"www-1(:[0-9]+)?"`, hostInstanceMatcher("node_name", "www-1"))
}

func TestExactInstanceMatcher(t *testing.T) {
	assert.Equal(t, `instance=~"^(a|b)$"`, exactInstanceMatcher("instance", []string{"a", "b"}))
	assert.Equal(t, `instance=~"^(www-1:9100)$"`, exactInstanceMatcher("instance", []string{"www-1:9100"}))
}

func TestWindowDescription(t *testing.T) {
	for _, tc := range []struct {
		from, to, want string
	}{
		{"now-1h", "now", "last hour"},
		{"now-1m", "now", "last minute"},
		{"now-6h", "now", "last 6 hours"},
		{"now-30m", "now", "last 30 minutes"},
		{"now-2d", "now", "last 2 days"},
		{"2026-01-01", "2026-01-02", "2026-01-01 to 2026-01-02"},
		{"now-6h", "now-1h", "now-6h to now-1h"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, windowDescription(tc.from, tc.to))
		})
	}
}

func TestHostMetricTitle(t *testing.T) {
	assert.Equal(t, "CPU on www-1 (last hour)", hostMetricTitle(metricCPU, "www-1", "now-1h", "now"))
	assert.Equal(t, "Memory on www-1 (last 6 hours)", hostMetricTitle(metricMemory, "www-1", "now-6h", "now"))
}

func TestHostDictionaryLabels(t *testing.T) {
	assert.Equal(t, []string{"instance", "nodename"}, hostDictionaryLabels("instance"))
	assert.Equal(t, []string{"custom", "nodename"}, hostDictionaryLabels("custom"))
	// The instance label itself can be configured to "nodename"; list_hosts must not then query
	// the same label twice.
	assert.Equal(t, []string{"nodename"}, hostDictionaryLabels("nodename"))
}

func TestSelectorEmpty(t *testing.T) {
	assert.Equal(t, "{}", selector())
	assert.Equal(t, "{}", selector("", ""))
	assert.Equal(t, `{a="1"}`, selector("", `a="1"`, ""))
	assert.Equal(t, `{a="1",b="2"}`, selector(`a="1"`, `b="2"`))
}

func TestJobMatcher(t *testing.T) {
	assert.Equal(t, "", jobMatcher(""))
	assert.Equal(t, `job="node"`, jobMatcher("node"))
	// A job filter value needs the same escaping as a host: it's admin-configured, not
	// person-supplied, but still ends up in the same kind of PromQL string literal.
	assert.Equal(t, `job="no\"de"`, jobMatcher(`no"de`))
}
