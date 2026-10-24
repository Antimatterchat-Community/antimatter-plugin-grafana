// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsBlockedAddress(t *testing.T) {
	for ip, blocked := range map[string]bool{
		// Link-local: blocked.
		"169.254.0.1":     true,
		"169.254.169.254": true, // the AWS/GCP/Azure/DigitalOcean/OpenStack metadata address
		"fe80::1":         true,
		"224.0.0.1":       true, // IPv4 link-local multicast
		"ff02::1":         true, // IPv6 link-local multicast

		// Explicit cloud metadata addresses not otherwise covered: blocked.
		"fd00:ec2::254":   true,
		"100.100.100.200": true,
		"192.0.0.192":     true,

		// Private, loopback and CGNAT: allowed, unlike a dialer that refuses them.
		"127.0.0.1":   false,
		"10.1.2.3":    false,
		"172.16.0.1":  false,
		"192.168.1.1": false,
		"100.64.0.1":  false,
		"::1":         false,
		"fd12::1":     false, // a unique-local address other than the AWS metadata one

		// Public: allowed.
		"93.184.215.14": false,
		"2606:4700::1":  false,
	} {
		assert.Equal(t, blocked, isBlockedAddress(net.ParseIP(ip)), ip)
	}
	assert.True(t, isBlockedAddress(nil))
}

func TestDialerRefusesLinkLocalAndMetadataButAllowsPrivate(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	// A loopback address, which is private: allowed.
	conn, err := newDialer().Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	_ = conn.Close()

	// A link-local address: refused, without ever reaching the network (the dialer's Control
	// callback runs before the connection completes).
	_, err = newDialer().Dial("tcp", "169.254.169.254:80")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBlockedAddress))
}
