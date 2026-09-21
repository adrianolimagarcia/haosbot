package netpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultMaxResponseBytes int64 = 4 << 20 // 4 MiB

// Policy defines the outbound HTTP boundary shared by tools and control-plane
// handlers. Private/loopback/link-local destinations are denied unless they are
// explicitly listed in Allowlist.
type Policy struct {
	Allowlist        []string
	MaxResponseBytes int64
}

func (p Policy) maxResponseBytes() int64 {
	if p.MaxResponseBytes <= 0 {
		return DefaultMaxResponseBytes
	}
	return p.MaxResponseBytes
}

// ValidateURL validates the URL syntax/scheme and resolves the current hostname
// so callers fail before starting a request. The transport performs the same
// validation again at dial time to prevent DNS rebinding/TOCTOU bypasses.
func ValidateURL(ctx context.Context, raw string, p Policy) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("URL host is required")
	}
	if u.User != nil {
		return nil, errors.New("URL userinfo is not allowed")
	}
	if err := validateHost(ctx, u.Hostname(), p.Allowlist); err != nil {
		return nil, err
	}
	return u, nil
}

// NewClient returns an HTTP client whose dialer validates the final IP address
// and whose redirect policy re-validates every hop. This closes the common
// validate-then-re-resolve DNS rebinding gap.
func NewClient(timeout time.Duration, p Policy) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", address, err)
		}

		ips, err := resolveHost(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !allowedHostIP(host, ip, p.Allowlist) && blockedIP(ip) {
				return nil, fmt.Errorf("outbound destination %s resolves to blocked address %s", host, ip)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("outbound destination %s resolved to no addresses", host)
		}

		// Dial the addresses validated above, in order, until one connects. Trying
		// only the first is wrong for a dual-stack name: "localhost" commonly
		// resolves to ::1 first while the peer listens on IPv4 only, so the request
		// fails even though a later address would have served it. TLS still uses the
		// original request hostname for SNI and certificate verification.
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("outbound destination %s: %w", host, lastErr)
	}

	client := &http.Client{
		Transport: base,
		Timeout:   timeout,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		_, err := ValidateURL(req.Context(), req.URL.String(), p)
		return err
	}
	return client
}

func validateHost(ctx context.Context, host string, allowlist []string) error {
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("outbound destination %s resolved to no addresses", host)
	}
	for _, ip := range ips {
		if allowedHostIP(host, ip, allowlist) {
			continue
		}
		if blockedIP(ip) {
			return fmt.Errorf("outbound destination %s resolves to blocked address %s", host, ip)
		}
	}
	return nil
}

func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	trimmed := strings.Trim(host, "[]")
	if ip := net.ParseIP(trimmed); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, trimmed)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", trimmed, err)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP != nil {
			ips = append(ips, addr.IP)
		}
	}
	return ips, nil
}

func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

func allowedHostIP(host string, ip net.IP, allowlist []string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	for _, raw := range allowlist {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if u, err := url.Parse(entry); err == nil && u.Hostname() != "" {
			entry = u.Hostname()
		}
		if h, _, err := net.SplitHostPort(entry); err == nil {
			entry = h
		}
		entry = strings.Trim(entry, "[]")

		if strings.EqualFold(entry, host) {
			return true
		}
		if allowIP := net.ParseIP(entry); allowIP != nil && allowIP.Equal(ip) {
			return true
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// MaxResponseBytes returns the configured response budget for callers that use
// io.LimitReader/MaxBytesReader around response bodies.
func MaxResponseBytes(p Policy) int64 { return p.maxResponseBytes() }
