// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// communityAllowed tells whether a community (by its ID, name or display name) is in an allowed
// list, matching case-insensitively.
func communityAllowed(teamID string, team *model.Team, allowed []string) bool {
	for _, entry := range allowed {
		if strings.EqualFold(entry, teamID) {
			return true
		}
		if team != nil && (strings.EqualFold(entry, team.Name) || strings.EqualFold(entry, team.DisplayName)) {
			return true
		}
	}
	return false
}

// authorizeToolCall checks that the person who made an AI tool call may use the Grafana tools
// where they made it: their community is allowed by the app's settings, and they themselves may
// see that channel or community (never more than what they could already see). It returns an
// empty string when the call may go ahead, or a reason (safe to show the person) otherwise.
//
// This app's tools don't take a channel or community argument the server's own permission
// mechanism (AIToolDefinition.Permissions) could check: they always act on the conversation the
// call happened in, which the server gives as call.ChannelId/call.TeamId rather than as a tool
// argument. So the check is done here instead, with that context.
func (p *Plugin) authorizeToolCall(cfg *configuration, call *model.AIToolExecution) string {
	// The community-list check only runs, and only resolves the channel's community, when the
	// settings actually restrict the tools to specific communities: a server that lets every
	// community use them (the default) doesn't need the extra GetChannel/GetTeam calls.
	if !cfg.AllCommunities {
		teamID := call.TeamId
		if teamID == "" && call.ChannelId != "" {
			channel, appErr := p.API.GetChannel(call.ChannelId)
			if appErr != nil {
				return "Couldn't check whether you may use this tool here."
			}
			teamID = channel.TeamId
		}
		if teamID == "" {
			// No community to check against (for instance, a direct message): refuse rather than
			// guess.
			return "This app's Grafana tools are limited to specific communities, and this conversation isn't in one."
		}
		team, appErr := p.API.GetTeam(teamID)
		if appErr != nil {
			return "Couldn't check whether you may use this tool here."
		}
		if !communityAllowed(teamID, team, cfg.allowedCommunities()) {
			return "This app's Grafana tools aren't allowed for this community."
		}
	}

	// Whether or not every community may use the tools, the person themselves must be able to
	// see where they're asking from: never answer with more than they could already see.
	switch {
	case call.ChannelId != "":
		if ok := p.API.HasPermissionToChannel(call.UserId, call.ChannelId, model.PermissionReadChannel); !ok {
			return "You don't have access to this channel."
		}
	case call.TeamId != "":
		if ok := p.API.HasPermissionToTeam(call.UserId, call.TeamId, model.PermissionViewTeam); !ok {
			return "You aren't a member of this community."
		}
	default:
		return "Couldn't check whether you may use this tool here."
	}
	return ""
}
