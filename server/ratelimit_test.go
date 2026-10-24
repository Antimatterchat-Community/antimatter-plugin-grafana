// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestToolLimiterBurstThenRefill(t *testing.T) {
	l := newToolLimiter()
	now := time.Now()

	for i := range toolRateBurst {
		assert.True(t, l.allow("user1", now), "call %d of the burst should be allowed", i)
	}
	assert.False(t, l.allow("user1", now), "the burst is spent")

	// After one full interval, exactly one more token is available.
	now = now.Add(toolRateInterval)
	assert.True(t, l.allow("user1", now))
	assert.False(t, l.allow("user1", now))

	// A different person has their own, untouched bucket.
	assert.True(t, l.allow("user2", now))
}
