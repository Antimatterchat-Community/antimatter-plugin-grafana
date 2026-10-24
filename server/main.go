// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"github.com/mattermost/mattermost/server/public/plugin"
)

func main() {
	plugin.ClientMain(&Plugin{})
}
