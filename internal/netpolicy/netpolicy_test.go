package netpolicy

import (
	"context"
	"net"
	"testing"
)

func TestValidateURLRejectsPrivateAndLoopbackIPs(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"http://127.0.0.1:8080/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
	} {
		if _, err := ValidateURL(ctx, raw, Policy{}); err == nil {
			t.Fatalf("ValidateURL(%q) expected rejection", raw)
		}
	}
}

func TestValidateURLAllowlistSupportsExactIPAndCIDR(t *testing.T) {
	ctx := context.Background()

	if _, err := ValidateURL(ctx, "http://127.0.0.1:8080/", Policy{Allowlist: []string{"127.0.0.1"}}); err != nil {
		t.Fatalf("exact allowlist should permit loopback: %v", err)
	}
	if _, err := ValidateURL(ctx, "http://10.12.1.7/", Policy{Allowlist: []string{"10.12.0.0/16"}}); err != nil {
		t.Fatalf("CIDR allowlist should permit matching private IP: %v", err)
	}
}

func TestValidateURLRejectsUnsupportedSchemeAndUserinfo(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"file:///etc/passwd",
		"ftp://example.com/file",
		"http://user:pass@8.8.8.8/",
	} {
		if _, err := ValidateURL(ctx, raw, Policy{}); err == nil {
			t.Fatalf("ValidateURL(%q) expected rejection", raw)
		}
	}
}

func TestBlockedIPClassification(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}
	for _, tc := range cases {
		if got := blockedIP(net.ParseIP(tc.ip)); got != tc.blocked {
			t.Fatalf("blockedIP(%s)=%v want %v", tc.ip, got, tc.blocked)
		}
	}
}
