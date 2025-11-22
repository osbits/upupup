package access

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Allowlist controls which IP addresses can access the server.
type Allowlist struct {
	networks      []*net.IPNet
	allowAll      bool
	allowedIPsStr map[string]struct{}
}

// NewAllowlist constructs an allowlist from CIDR or explicit IP strings.
func NewAllowlist(entries []string) (*Allowlist, error) {
	if len(entries) == 0 {
		return &Allowlist{allowAll: true}, nil
	}
	al := &Allowlist{
		networks:      make([]*net.IPNet, 0, len(entries)),
		allowedIPsStr: make(map[string]struct{}),
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, network, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("parse cidr %q: %w", entry, err)
			}
			al.networks = append(al.networks, network)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("parse ip %q: invalid address", entry)
		}
		al.allowedIPsStr[ip.String()] = struct{}{}
	}
	if len(al.networks) == 0 && len(al.allowedIPsStr) == 0 {
		al.allowAll = true
	}
	return al, nil
}

// Allowed reports whether the provided IP is permitted.
func (a *Allowlist) Allowed(ip net.IP) bool {
	if a == nil || a.allowAll {
		return true
	}
	if ip == nil {
		return false
	}
	if _, ok := a.allowedIPsStr[ip.String()]; ok {
		return true
	}
	for _, network := range a.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIPFromRequest returns the best-effort client IP based on headers and remote address.
func ClientIPFromRequest(r *http.Request, trusted []*net.IPNet) (net.IP, string) {
	if r == nil {
		return nil, ""
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		if len(trusted) == 0 || lastHopTrusted(r.RemoteAddr, trusted) {
			parts := strings.Split(xff, ",")
			for _, part := range parts {
				ipStr := strings.TrimSpace(part)
				if ipStr == "" {
					continue
				}
				ip := net.ParseIP(ipStr)
				if ip == nil {
					continue
				}
				return ip, ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, host
	}
	return ip, ip.String()
}

func lastHopTrusted(remoteAddr string, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// ParseCIDRs converts cidr strings to IP network list.
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var result []*net.IPNet
	for _, entry := range cidrs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("parse cidr %q: %w", entry, err)
		}
		result = append(result, network)
	}
	return result, nil
}
