package telegram

import (
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// A faithful subset of urllib.parse.urlsplit, plus the two NetlocResultMixins
// properties the Telegram validators read (hostname, port).
//
// WHY THIS EXISTS. Both places that parse a URL in this package —
// _proxy_url_is_valid and validate_webhook_config — depend on details of
// CPython's parser that net/url does not share, and the reference's behaviour is
// observable in the corpus:
//
//   - only the LEADING C0-control-or-space run is stripped, so a trailing space
//     stays in the netloc and turns a valid port into a port-parsing error;
//   - TAB, CR and LF are deleted from ANYWHERE in the URL before parsing, so
//     "https:\t//host" has a netloc and "https://ho\nst" parses;
//   - the netloc is split after "//" for EVERY scheme, not only for the schemes
//     in urllib's uses_netloc list;
//   - a netloc containing "[" without "]" (or the reverse) raises
//     ValueError("Invalid IPv6 URL"), and a bracketed host is validated as an
//     IP literal;
//   - a non-ASCII netloc is rejected when NFKC normalisation would introduce
//     one of "/?#@:" (the homograph guard);
//   - `port` accepts leading zeros ("080") and rejects non-digits, signs,
//     spaces, anything above 65535 and anything but a plain integer.
//
// The two properties are implemented as hostnameOf/portOf rather than as a
// struct because that is the only shape the callers need.

// URL parse errors, with urllib's exact message text. validate_webhook_config
// does not catch them, so they reach the caller as pydantic value errors and the
// text is part of the observable contract.
var (
	// ErrInvalidIPv6URL is urlsplit's `raise ValueError("Invalid IPv6 URL")`.
	ErrInvalidIPv6URL = errors.New("Invalid IPv6 URL")
	// ErrInvalidIPvFuture is _check_bracketed_host's IPvFuture message.
	ErrInvalidIPvFuture = errors.New("IPvFuture address is invalid")
	// ErrIPv4InBrackets is _check_bracketed_host's IPv4 message.
	ErrIPv4InBrackets = errors.New("An IPv4 address cannot be in brackets")
)

// errNotAnIPAddress is ipaddress's AddressValueError text.
func errNotAnIPAddress(host string) error {
	return errors.New("'" + host + "' does not appear to be an IPv4 or IPv6 address")
}

// errNFKCNeloc is _checknetloc's message.
func errNFKCNetloc(netloc string) error {
	return errors.New("netloc '" + netloc + "' contains invalid characters under NFKC normalization")
}

// schemeChars is urllib.parse.scheme_chars.
const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

// nfkcDangerousCodePoints is the set of code points whose NFKC normalisation
// introduces one of "/?#@:".
//
// _checknetloc raises when NFKC(netloc minus "@:#?") still contains any of those
// five characters. Since NFC composition can never CREATE one of them from
// non-ASCII input, the check is equivalent to "the netloc contains one of these
// code points". The set was obtained by ENUMERATING the reference interpreter:
//
//	[cp for cp in range(0x110000)
//	 if not chr(cp).isascii()
//	 and any(b in unicodedata.normalize('NFKC', chr(cp)) for b in '/?#@:')]
//
// and it has exactly 19 members.
var nfkcDangerousCodePoints = []rune{
	0x2047, 0x2048, 0x2049, // ⁇ ⁈ ⁉  -> "??", "?!", "!?"
	0x2100, 0x2101, 0x2105, 0x2106, // ℀ ℁ ℅ ℆ -> "a/c", "a/s", "c/o", "c/u"
	0x2A74,                         // ⩴ -> "::="
	0xFE13, 0xFE16, 0xFE55, 0xFE56, // presentation forms -> ":" and "?"
	0xFE5F, 0xFE6B, // "#" and "@"
	0xFF03, 0xFF0F, 0xFF1A, 0xFF1F, 0xFF20, // fullwidth # / : ? @
}

// pyURLSplit is urllib.parse.urlsplit restricted to the scheme and netloc it
// computes, and to the ValueErrors it can raise.
func pyURLSplit(raw string) (scheme, netloc string, err error) {
	url := strings.TrimLeftFunc(raw, func(r rune) bool { return r >= 0 && r <= 0x20 })
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)

	if i := strings.IndexByte(url, ':'); i > 0 && url[0] < 0x80 && isASCIIAlpha(url[0]) && schemePrefixOK(url[:i]) {
		scheme = strings.ToLower(url[:i])
		url = url[i+1:]
	}

	if strings.HasPrefix(url, "//") {
		netloc, _ = splitNetloc(url, 2)
		if (strings.Contains(netloc, "[") && !strings.Contains(netloc, "]")) ||
			(strings.Contains(netloc, "]") && !strings.Contains(netloc, "[")) {
			return "", "", ErrInvalidIPv6URL
		}
		if strings.Contains(netloc, "[") && strings.Contains(netloc, "]") {
			if err := checkBracketedNetloc(netloc); err != nil {
				return "", "", err
			}
		}
	}
	if err := checkNetloc(netloc); err != nil {
		return "", "", err
	}
	return scheme, netloc, nil
}

// splitNetloc is urllib.parse._splitnetloc: everything from start up to the
// earliest of '/', '?' or '#'.
func splitNetloc(url string, start int) (netloc, rest string) {
	delim := len(url)
	for _, c := range []byte{'/', '?', '#'} {
		if i := strings.IndexByte(url[start:], c); i >= 0 && start+i < delim {
			delim = start + i
		}
	}
	return url[start:delim], url[delim:]
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func schemePrefixOK(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(schemeChars, rune(s[i])) {
			return false
		}
	}
	return true
}

// checkNetloc is urllib.parse._checknetloc.
func checkNetloc(netloc string) error {
	if netloc == "" || isASCIIString(netloc) {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	for _, r := range n {
		for _, dangerous := range nfkcDangerousCodePoints {
			if r == dangerous {
				return errNFKCNetloc(netloc)
			}
		}
	}
	return nil
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// checkBracketedNetloc is urllib.parse._check_bracketed_netloc.
func checkBracketedNetloc(netloc string) error {
	hostnameAndPort := rpartitionAfter(netloc, '@')
	beforeBracket, haveOpenBr, bracketed := partition(hostnameAndPort, '[')
	var hostname string
	if haveOpenBr {
		if beforeBracket != "" {
			return ErrInvalidIPv6URL
		}
		var port string
		hostname, _, port = partition(bracketed, ']')
		if port != "" && !strings.HasPrefix(port, ":") {
			return ErrInvalidIPv6URL
		}
	} else {
		hostname, _, _ = partition(hostnameAndPort, ':')
	}
	return checkBracketedHost(hostname)
}

var ipvFutureRe = regexp.MustCompile(`^v[0-9A-Fa-f]+\..+$`)

// checkBracketedHost is urllib.parse._check_bracketed_host.
//
// DIVERGENCE (documented): Python's ipaddress module accepts a scope id on an
// IPv6 literal ("fe80::1%25eth0") and rejects IPv4 with one; Go's net.ParseIP
// rejects any '%'. The scope is therefore split off before parsing, which
// reproduces Python for every case in the corpus. Python's ipaddress is also
// stricter than net.ParseIP about a few legacy IPv4 spellings; the corpus pins
// the bracketed forms that matter.
func checkBracketedHost(hostname string) error {
	if strings.HasPrefix(hostname, "v") {
		if !ipvFutureRe.MatchString(hostname) {
			return ErrInvalidIPvFuture
		}
		return nil
	}
	address := hostname
	if i := strings.IndexByte(address, '%'); i >= 0 {
		address = address[:i]
	}
	if net.ParseIP(address) == nil {
		return errNotAnIPAddress(hostname)
	}
	// Python distinguishes the two families by the INPUT, not by the parsed
	// value: "::ffff:1.2.3.4" is an IPv6Address and is allowed in brackets,
	// while "1.2.3.4" is an IPv4Address and is not.
	if !strings.Contains(address, ":") {
		return ErrIPv4InBrackets
	}
	return nil
}

// hostnameOf is SplitResult.hostname: the host part of the netloc, lower-cased,
// with the scope id preserved, or nil when there is no host.
func hostnameOf(netloc string) *string {
	hostinfo := rpartitionAfter(netloc, '@')
	var hostname string
	if idx := strings.IndexByte(hostinfo, '['); idx >= 0 {
		hostname, _, _ = partition(hostinfo[idx+1:], ']')
	} else {
		hostname, _, _ = partition(hostinfo, ':')
	}
	if hostname == "" {
		return nil
	}
	base, hasPercent, zone := partition(hostname, '%')
	lowered := strings.ToLower(base)
	if hasPercent {
		// The scope id is deliberately NOT lower-cased, matching CPython:
		// http://[fe80::1%tESt]:80 keeps the case of "tESt".
		lowered += "%" + zone
	}
	return &lowered
}

// portOf is SplitResult.port, including its two ValueErrors.
func portOf(netloc string) (*int, error) {
	hostinfo := rpartitionAfter(netloc, '@')
	var port string
	if idx := strings.IndexByte(hostinfo, '['); idx >= 0 {
		bracketed := hostinfo[idx+1:]
		_, _, afterBracket := partition(bracketed, ']')
		_, _, port = partition(afterBracket, ':')
	} else {
		_, _, port = partition(hostinfo, ':')
	}
	if port == "" {
		return nil, nil
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return nil, errors.New("Port could not be cast to integer value as " + pyRepr(port))
		}
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 0 || value > 65535 {
		return nil, errors.New("Port out of range 0-65535")
	}
	return &value, nil
}

// pyRepr renders a Python str repr for the port error message.
func pyRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r == rune(quote[0]) {
				b.WriteString("\\" + quote)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// partition is str.partition: (before, sep, after), with after == "" when sep is
// absent.
func partition(s string, sep byte) (before string, found bool, after string) {
	i := strings.IndexByte(s, sep)
	if i < 0 {
		return s, false, ""
	}
	return s[:i], true, s[i+1:]
}

// rpartitionAfter is the `after` half of str.rpartition('@'), which is what
// _hostinfo uses: everything after the LAST '@', or the whole string.
func rpartitionAfter(s string, sep byte) string {
	if i := strings.LastIndexByte(s, sep); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---------------------------------------------------------------------------
// The two validators that consume the parser
// ---------------------------------------------------------------------------

// supportedProxySchemes is _SUPPORTED_PROXY_SCHEMES (validation.py:21).
var supportedProxySchemes = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

// ProxyURLIsValid is _proxy_url_is_valid (validation.py:24-35).
//
// A scheme-less value is prefixed with "http://", which is why a bare
// "proxy.local:8080" is accepted and a bare " " is too (the space becomes the
// host). Any ValueError from urlparse — including the port property — means
// invalid; the scheme test is case-insensitive because urlparse lower-cases it.
func ProxyURLIsValid(proxy string) bool {
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	scheme, netloc, err := pyURLSplit(proxy)
	if err != nil {
		return false
	}
	hostname := hostnameOf(netloc)
	if _, err := portOf(netloc); err != nil {
		return false
	}
	return supportedProxySchemes[strings.ToLower(scheme)] && hostname != nil
}
