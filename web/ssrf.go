package web

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// The SSRF guard of Node.js WeKan (models/lib/attachmentUrlValidation.js,
// GHSA-hc3x-hq3m-663q): a URL the server itself will call - a webhook - must be
// http or https to a host that is not this machine, a private network, a
// link-local or metadata address, or one of those wrapped in an IPv6 address.

var blockedHostnames = map[string]bool{"localhost": true, "localhost.localdomain": true, "ip6-localhost": true,
	"ip6-loopback": true, "0": true, "0.0.0.0": true}

var blockedIPv4 = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

func ipv4Blocked(a netip.Addr) bool {
	for _, p := range blockedIPv4 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// embeddedIPv4 is every IPv4 address an IPv6 address may carry: mapped,
// compatible, 6to4, NAT64 (RFC 6052 at each prefix length), Teredo (server and
// client) and ISATAP - the same list as WeKan's.
func embeddedIPv4(b [16]byte) []netip.Addr {
	v4 := func(x [4]byte) netip.Addr { return netip.AddrFrom4(x) }
	at := func(i int) netip.Addr { return v4([4]byte{b[i], b[i+1], b[i+2], b[i+3]}) }
	zero := func(from, to int) bool {
		for _, x := range b[from:to] {
			if x != 0 {
				return false
			}
		}
		return true
	}
	var out []netip.Addr
	if zero(0, 10) && b[10] == 0xff && b[11] == 0xff {
		out = append(out, at(12))
	}
	if zero(0, 12) {
		out = append(out, at(12))
	}
	if zero(0, 8) && b[8] == 0xff && b[9] == 0xff && zero(10, 12) {
		out = append(out, at(12))
	}
	if b[0] == 0x20 && b[1] == 0x02 {
		out = append(out, at(2))
	}
	if b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b {
		if zero(4, 12) {
			out = append(out, at(12))
		} else {
			for _, pos := range [][4]int{{4, 5, 6, 7}, {5, 6, 7, 9}, {6, 7, 9, 10}, {7, 9, 10, 11}, {9, 10, 11, 12}, {12, 13, 14, 15}} {
				out = append(out, v4([4]byte{b[pos[0]], b[pos[1]], b[pos[2]], b[pos[3]]}))
			}
		}
	}
	if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x00 && b[3] == 0x00 {
		out = append(out, at(4), v4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}))
	}
	if (b[8] == 0x00 || b[8] == 0x02) && b[9] == 0x00 && b[10] == 0x5e && b[11] == 0xfe {
		out = append(out, at(12))
	}
	return out
}

// ipBlocked is WeKan's isIpBlocked.
func ipBlocked(ip string) bool {
	a, err := netip.ParseAddr(strings.Trim(ip, "[]"))
	if err != nil {
		return true
	}
	a = a.WithZone("")
	if a.Is4() {
		return ipv4Blocked(a)
	}
	b := a.As16()
	for _, e := range embeddedIPv4(b) {
		if ipv4Blocked(e) {
			return true
		}
	}
	switch {
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x0d && b[3] == 0xb8: // documentation
		return true
	case b[0] == 0xfc || b[0] == 0xfd: // unique local
		return true
	case b[0] == 0xfe && (b[1]&0xc0 == 0x80 || b[1]&0xc0 == 0xc0): // link-local, site-local
		return true
	case b[0] == 0xff: // multicast
		return true
	}
	return false
}

// lookupHost resolves a host name; a variable so a test needs no network.
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// validateOutboundURL is WeKan's validateAttachmentUrl: "" when the URL may be
// called, else why not.
func validateOutboundURL(ctx context.Context, raw string) string {
	if raw == "" {
		return "Empty URL"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" && u.Scheme == "" {
		return "Invalid URL format"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "Only HTTP and HTTPS protocols are allowed"
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "Missing hostname"
	}
	if blockedHostnames[host] || strings.HasSuffix(host, ".localhost") {
		return "Localhost is not allowed"
	}
	if _, err := netip.ParseAddr(host); err == nil {
		if ipBlocked(host) {
			return "IP address is not allowed"
		}
		return ""
	}
	addrs, err := lookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return "Hostname did not resolve"
	}
	for _, a := range addrs {
		if ipBlocked(a) {
			return "Resolved IP address is not allowed"
		}
	}
	return ""
}
