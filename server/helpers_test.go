// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// testToken is the fake service account token used throughout the tests. Every test that
// exercises a code path which could leak it must assert it never appears in a result, an error or
// a logged message.
const testToken = "glsa_super-secret-service-account-token"

// testEnv bundles a Plugin wired to a mocked plugin API, for the AI tool tests.
type testEnv struct {
	p   *Plugin
	api *plugintest.API
}

// newTestEnv returns a Plugin ready to run tool calls, with its configuration pointed at
// grafanaURL (normally an httptest server) and every log level stubbed out so a test can assert
// on specific log calls if it wants to (logCapture) without every other test having to expect
// them.
func newTestEnv(t *testing.T, grafanaURL string, cfg configuration) *testEnv {
	t.Helper()
	api := &plugintest.API{}
	t.Cleanup(func() { api.AssertExpectations(t) })

	cfg.GrafanaURL = grafanaURL
	if cfg.ServiceAccountToken == "" {
		cfg.ServiceAccountToken = testToken
	}
	p := &Plugin{
		MattermostPlugin: plugin.MattermostPlugin{API: api},
		configuration:    &cfg,
		toolLimiter:      newToolLimiter(),
		renderer:         &rendererDetector{},
	}
	return &testEnv{p: p, api: api}
}

// expectLogs stubs every log level as allowed (zero or more times), optionally running fn with
// the logged key/value pairs for every LogWarn call, so a test can inspect what was logged
// without having to predict the exact arguments.
func (e *testEnv) expectLogs(onWarn func(args ...any)) {
	for _, level := range []string{"LogDebug", "LogInfo", "LogError"} {
		e.api.On(level, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		e.api.On(level, mock.Anything, mock.Anything, mock.Anything).Maybe()
		e.api.On(level, mock.Anything).Maybe()
	}
	call := e.api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	if onWarn != nil {
		call.Run(func(args mock.Arguments) { onWarn(args...) })
	}
}

// allowChannel makes the mocked API say userID may read channelID, in a community that's allowed
// (teamID, possibly "").
func (e *testEnv) allowChannel(userID, channelID, teamID string) {
	e.api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: teamID}, nil).Maybe()
	e.api.On("HasPermissionToChannel", userID, channelID, model.PermissionReadChannel).Return(true).Maybe()
}

// grafanaFake starts an httptest server for a test's handler to inspect each request against
// (path, query, headers, body) and to respond as a fake Grafana would.
type grafanaFake struct {
	*httptest.Server
}

func newGrafanaFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *grafanaFake {
	t.Helper()
	fake := &grafanaFake{Server: httptest.NewServer(http.HandlerFunc(handler))}
	t.Cleanup(fake.Close)
	return fake
}

// decodeToolResult decodes a tool's result (an AIToolResult.Text that must not be an error) as the
// attachments/summary envelope display.go's handlers encode.
func decodeToolResult(t *testing.T, result *model.AIToolResult) toolResult {
	t.Helper()
	require.Empty(t, result.Error)
	var out toolResult
	require.NoError(t, json.Unmarshal([]byte(result.Text), &out), result.Text)
	return out
}

// fieldValue gives the string value of one of an attachment's fields, "" when it has none of that
// title.
func fieldValue(t *testing.T, a *model.MessageAttachment, title string) string {
	t.Helper()
	for _, f := range a.Fields {
		if f.Title == title {
			v, _ := f.Value.(string)
			return v
		}
	}
	return ""
}
