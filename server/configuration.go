// Copyright (c) 2019-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/pkg/errors"
)

// configuration captures the plugin's external configuration as exposed in the Antimatter server
// configuration, as well as values computed from the configuration. Any public fields will be
// deserialized from the Antimatter server configuration in OnConfigurationChange.
//
// As plugins are inherently concurrent (hooks being called asynchronously), and the plugin
// configuration can change at any time, access to the configuration must be synchronized. The
// strategy used in this plugin is to guard a pointer to the configuration, and clone the entire
// struct whenever it changes. You may replace this with whatever strategy you choose.
type configuration struct {
	// GrafanaURL is the URL this server uses to reach Grafana, such as "http://grafana:3000".
	// Unlike some other apps' server settings, private addresses are allowed here (Grafana is
	// usually on the internal network); link-local and cloud metadata addresses are still refused.
	GrafanaURL string

	// GrafanaPublicURL, when set, replaces GrafanaURL in the links a tool's result gives (the
	// dashboard or Explore link), for when members' browsers reach Grafana at a different address
	// than this server does. It is never used for the requests themselves.
	GrafanaPublicURL string

	// ServiceAccountToken authenticates to Grafana. A Viewer-rights token is enough. It is never
	// sent to the web app and never logged.
	ServiceAccountToken string

	// AllCommunities lets every community use the Grafana tools.
	AllCommunities bool

	// AllowedCommunities, when AllCommunities is false, lists the communities (one name or ID per
	// line) whose members may use the Grafana tools. Ignored when AllCommunities is true.
	AllowedCommunities string
}

// String redacts the service account token, so an accidental %v/%+v of the configuration (in a
// log line, for instance) never leaks it.
func (c *configuration) String() string {
	if c == nil {
		return "<nil>"
	}
	token := ""
	if c.ServiceAccountToken != "" {
		token = "<redacted>"
	}
	return fmt.Sprintf(
		"configuration{GrafanaURL:%q GrafanaPublicURL:%q ServiceAccountToken:%q AllCommunities:%v AllowedCommunities:%q}",
		c.GrafanaURL, c.GrafanaPublicURL, token, c.AllCommunities, c.AllowedCommunities,
	)
}

// grafanaURL returns the URL this server uses to reach Grafana, trimmed of a trailing slash.
func (c *configuration) grafanaURL() string {
	return strings.TrimRight(strings.TrimSpace(c.GrafanaURL), "/")
}

// publicURL returns the URL to use in links a tool's result gives: GrafanaPublicURL if set, else
// GrafanaURL.
func (c *configuration) publicURL() string {
	if url := strings.TrimRight(strings.TrimSpace(c.GrafanaPublicURL), "/"); url != "" {
		return url
	}
	return c.grafanaURL()
}

// allowedCommunities parses AllowedCommunities into its trimmed, non-empty lines.
func (c *configuration) allowedCommunities() []string {
	var out []string
	for line := range strings.SplitSeq(c.AllowedCommunities, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Clone shallow copies the configuration. Your implementation may require a deep copy if
// your configuration has reference types.
func (c *configuration) Clone() *configuration {
	clone := *c
	return &clone
}

// getConfiguration retrieves the active configuration under lock, making it safe to use
// concurrently. The active configuration may change underneath the client of this method, but
// the struct returned by this API call is considered immutable.
func (p *Plugin) getConfiguration() *configuration {
	p.configurationLock.RLock()
	defer p.configurationLock.RUnlock()

	if p.configuration == nil {
		return &configuration{}
	}

	return p.configuration
}

// setConfiguration replaces the active configuration under lock.
//
// Do not call setConfiguration while holding the configurationLock, as sync.Mutex is not
// reentrant. In particular, avoid using the plugin API entirely, as this may in turn trigger a
// hook back into the plugin. If that hook attempts to acquire this lock, a deadlock may occur.
//
// This method panics if setConfiguration is called with the existing configuration. This almost
// certainly means that the configuration was modified without being cloned and may result in
// an unsafe access.
func (p *Plugin) setConfiguration(configuration *configuration) {
	p.configurationLock.Lock()
	defer p.configurationLock.Unlock()

	if configuration != nil && p.configuration == configuration {
		// Ignore assignment if the configuration struct is empty. Go will optimize the
		// allocation for same to point at the same memory address, breaking the check
		// above.
		if reflect.ValueOf(*configuration).NumField() == 0 {
			return
		}

		panic("setConfiguration called with the existing configuration")
	}

	p.configuration = configuration
}

// OnConfigurationChange is invoked when configuration changes may have been made.
func (p *Plugin) OnConfigurationChange() error {
	configuration := new(configuration)

	// Load the public configuration fields from the Antimatter server configuration.
	if err := p.API.LoadPluginConfiguration(configuration); err != nil {
		return errors.Wrap(err, "failed to load plugin configuration")
	}

	p.setConfiguration(configuration)

	return nil
}
