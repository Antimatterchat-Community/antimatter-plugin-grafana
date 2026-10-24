// Copyright (c) 2026-present Antimatter contributors.
// See LICENSE.txt for license information.

package main

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// dialTimeout bounds how long connecting to Grafana may take; requestTimeout (client.go) bounds
// the whole request.
const (
	dialTimeout = 5 * time.Second
	keepAlive   = 30 * time.Second
)

var errBlockedAddress = errors.New("connecting to that address isn't allowed on this server")

// metadataIPs are cloud metadata service addresses that aren't covered by the link-local ranges
// below: AWS's IPv6 metadata address (a unique-local address, which, unlike link-local addresses,
// is otherwise allowed here since Grafana itself is usually on a private network), Alibaba Cloud's
// metadata address (not in any reserved range) and Oracle Cloud's shared address space (RFC 7335,
// also not in any reserved range). The IPv4 metadata address every other cloud (AWS, GCP, Azure,
// DigitalOcean, OpenStack, ...) uses, 169.254.169.254, is already link-local and caught below.
var metadataIPs = map[string]bool{
	"fd00:ec2::254":   true, // AWS IMDSv2, IPv6
	"100.100.100.200": true, // Alibaba Cloud
	"192.0.0.192":     true, // Oracle Cloud
}

// isBlockedAddress returns whether an address is a link-local address (169.254.0.0/16,
// fe80::/10, and their multicast equivalents) or a known cloud metadata address. Unlike some other
// apps' dialers, private, loopback and carrier-grade-NAT addresses are deliberately NOT blocked
// here: Grafana is usually on the internal network the server itself runs in.
func isBlockedAddress(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return metadataIPs[ip.String()]
}

// newDialer returns the dialer used for every outbound request to Grafana. It refuses to connect
// to link-local or cloud metadata addresses. The check runs on the resolved address of each
// connection, so a hostname that resolves to one of them (such as a cloud metadata hostname) is
// refused too, whatever DNS name was used to reach it.
func newDialer() *net.Dialer {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || isBlockedAddress(ip) {
			return errBlockedAddress
		}
		return nil
	}
	return dialer
}
