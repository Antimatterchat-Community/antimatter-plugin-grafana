// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
)

func TestCommunityAllowed(t *testing.T) {
	team := &model.Team{Id: "team1", Name: "ops-team", DisplayName: "Ops Team"}
	assert.True(t, communityAllowed("team1", team, []string{"team1"}))
	assert.True(t, communityAllowed("team1", team, []string{"OPS-TEAM"}))
	assert.True(t, communityAllowed("team1", team, []string{"ops team"})) // display name, case-insensitive
	assert.False(t, communityAllowed("team1", team, []string{"other-team"}))
	assert.False(t, communityAllowed("team1", nil, []string{"ops-team"}))
}

func TestAuthorizeToolCall(t *testing.T) {
	const userID, channelID, teamID = "user1", "channel1", "team1"

	t.Run("all communities, channel allowed", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("HasPermissionToChannel", userID, channelID, model.PermissionReadChannel).Return(true)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: true}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, ChannelId: channelID})
		assert.Empty(t, reason)
	})

	t.Run("all communities, channel refused", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("HasPermissionToChannel", userID, channelID, model.PermissionReadChannel).Return(false)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: true}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, ChannelId: channelID})
		assert.NotEmpty(t, reason)
	})

	t.Run("restricted to communities, community not in the list", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: teamID}, nil)
		api.On("GetTeam", teamID).Return(&model.Team{Id: teamID, Name: "other"}, nil)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: false, AllowedCommunities: "ops-team"}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, ChannelId: channelID})
		assert.NotEmpty(t, reason)
	})

	t.Run("restricted to communities, community allowed and member", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: teamID}, nil)
		api.On("GetTeam", teamID).Return(&model.Team{Id: teamID, Name: "ops-team"}, nil)
		api.On("HasPermissionToChannel", userID, channelID, model.PermissionReadChannel).Return(true)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: false, AllowedCommunities: "ops-team"}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, ChannelId: channelID})
		assert.Empty(t, reason)
	})

	t.Run("restricted to communities, no community in the call (a DM)", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: false, AllowedCommunities: "ops-team"}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID})
		assert.NotEmpty(t, reason)
	})

	t.Run("belongs to an allowed community but can't see the channel itself", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: teamID}, nil)
		api.On("GetTeam", teamID).Return(&model.Team{Id: teamID, Name: "ops-team"}, nil)
		api.On("HasPermissionToChannel", userID, channelID, model.PermissionReadChannel).Return(false)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: false, AllowedCommunities: "ops-team"}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, ChannelId: channelID})
		assert.NotEmpty(t, reason)
	})

	t.Run("team id given directly, no channel", func(t *testing.T) {
		api := &plugintest.API{}
		defer api.AssertExpectations(t)
		api.On("HasPermissionToTeam", userID, teamID, model.PermissionViewTeam).Return(true)
		p := &Plugin{MattermostPlugin: plugin.MattermostPlugin{API: api}}
		cfg := &configuration{AllCommunities: true}
		reason := p.authorizeToolCall(cfg, &model.AIToolExecution{UserId: userID, TeamId: teamID})
		assert.Empty(t, reason)
	})
}
