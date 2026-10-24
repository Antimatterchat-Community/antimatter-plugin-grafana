// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"sync"

	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

// Plugin implements the interface expected by the Antimatter server to communicate between the
// server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	// toolLimiter limits how often each person may run this app's AI tools.
	toolLimiter *toolLimiter

	// renderer caches whether the grafana-image-renderer plugin is installed, for panel_data.
	renderer *rendererDetector
}

// OnActivate is invoked when the plugin is activated. It declares this app's AI tools; the
// server runs them through the ExecuteAITool hook (ai_tools.go).
func (p *Plugin) OnActivate() error {
	p.toolLimiter = newToolLimiter()
	p.renderer = &rendererDetector{}

	if appErr := p.API.RegisterAITools(aiToolDefinitions()); appErr != nil {
		return errors.Wrap(appErr, "failed to register the Grafana AI tools")
	}
	return nil
}
