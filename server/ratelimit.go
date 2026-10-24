// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"math"
	"sync"
	"time"
)

const (
	// toolRateBurst and toolRateInterval limit how often one person may run these tools: a burst
	// of 20, then one every 3 seconds (20 a minute), which comfortably covers normal assistant and
	// agent use while keeping a runaway loop from hammering Grafana.
	toolRateBurst    = 20
	toolRateInterval = 3 * time.Second
	// maxToolLimiterEntries bounds the limiter's memory: past it, idle people are forgotten.
	maxToolLimiterEntries = 10000
)

// toolLimiter limits how often each person runs this app's AI tools, in this server's memory
// (each server of a cluster counts its own): a token bucket per person.
type toolLimiter struct {
	mu      sync.Mutex
	buckets map[string]*toolBucket
}

type toolBucket struct {
	tokens float64
	last   time.Time
}

func newToolLimiter() *toolLimiter {
	return &toolLimiter{buckets: map[string]*toolBucket{}}
}

// allow takes one tool call for a person at now, and returns whether it may go ahead.
func (l *toolLimiter) allow(userID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[userID]
	if !ok {
		if len(l.buckets) >= maxToolLimiterEntries {
			l.sweep(now)
		}
		b = &toolBucket{tokens: toolRateBurst, last: now}
		l.buckets[userID] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(toolRateBurst, b.tokens+elapsed.Seconds()/toolRateInterval.Seconds())
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep forgets the people whose bucket is full again (nothing to remember), then, if that's not
// enough, any others: the limiter's memory stays bounded whatever is tried.
func (l *toolLimiter) sweep(now time.Time) {
	full := toolRateBurst * toolRateInterval
	for key, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, key)
		}
	}
	for key := range l.buckets {
		if len(l.buckets) < maxToolLimiterEntries/2 {
			break
		}
		delete(l.buckets, key)
	}
}
