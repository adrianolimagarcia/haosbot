module github.com/adrianolimagarcia/nanobot-go

go 1.25.0

// toolchain pins the patched Go used to BUILD and to scan for stdlib
// vulnerabilities, and it is the reason the security job can pass.
//
// The go directive alone is not enough: CI resolves actions/setup-go from
// go-version-file, and an explicit patch there installs EXACTLY that version —
// "go 1.25.0" therefore installed Go 1.25.0, whose standard library carries 18
// known advisories (crypto/tls, net/url, net/http, net/textproto, crypto/x509,
// encoding/asn1, net) that govulncheck reports as reachable from this code.
// setup-go gives the toolchain directive precedence over the go directive, so
// pinning here fixes CI and local builds from one place.
//
// go1.25.14 is the newest patch of the 1.25 line (1.25.15 does not exist).
// Verified: govulncheck ./... reports 0 reachable vulnerabilities with it, and
// the full test suite passes. Re-check this when bumping the minor version.
toolchain go1.25.14

require github.com/adrianolimagarcia/micrographrag-go v0.0.0-20260916231938-fdb8fd561f1c

require (
	github.com/asg017/sqlite-vec-go-bindings v0.0.0-20260326160809-b64d0e563e61 // indirect
	github.com/mattn/go-sqlite3 v1.14.49 // indirect
	github.com/nlpodyssey/safetensors v0.0.0-20250209183917-bfb01cc25f7c // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/tphakala/simd v1.3.0 // indirect
	github.com/trengrj/go-potion v0.0.0-20260823122308-c3ca68d3e5df // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	golang.org/x/text v0.25.0 // indirect
)
