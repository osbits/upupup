package access

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestAllowlistAllowAllWhenEmpty(t *testing.T) {
	al, err := NewAllowlist(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !al.Allowed(net.ParseIP("10.1.1.1")) {
		t.Fatalf("expected IP to be allowed by default")
	}
}

func TestAllowlistWithSpecificEntries(t *testing.T) {
	al, err := NewAllowlist([]string{"192.168.1.10", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !al.Allowed(net.ParseIP("192.168.1.10")) {
		t.Errorf("expected exact IP to be allowed")
	}
	if !al.Allowed(net.ParseIP("10.10.10.10")) {
		t.Errorf("expected CIDR to allow IP")
	}
	if al.Allowed(net.ParseIP("172.16.0.1")) {
		t.Errorf("unexpected allowance for IP outside of allowlist")
	}
}

func TestClientIPFromRequestRespectsXForwardedFor(t *testing.T) {
	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	req.RemoteAddr = "10.0.0.10:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.10")

	trusted, err := ParseCIDRs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("failed to parse cidr: %v", err)
	}
	ip, ipStr := ClientIPFromRequest(req, trusted)
	if ip == nil {
		t.Fatalf("expected client IP to be resolved")
	}
	if got := ip.String(); got != "203.0.113.5" {
		t.Fatalf("expected 203.0.113.5, got %s (%s)", got, ipStr)
	}

	// Without trusting proxy we should fall back to remote addr.
	ip2, _ := ClientIPFromRequest(req, nil)
	if got := ip2.String(); got != "203.0.113.5" {
		t.Fatalf("expected leftmost XFF even without trust due to empty trusted list, got %s", got)
	}
}
