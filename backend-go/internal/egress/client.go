// Package egress provides an exact HTTP(S)-origin dispatch boundary.
//
// Origins are trusted administrator configuration, not user input. An explicitly
// configured origin may intentionally name a LAN or loopback service. DNS follows
// that configured hostname, including subsequent DNS changes; this is NOT a global
// private-address filter or protection against an administrator trusting hostile
// DNS. No deployment origin, environment proxy, or ambient client is inherited.
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Policy and dispatch errors contain fixed symbols only. In particular, native
// http.Client errors are not exposed: their *url.Error includes the full URL.
var (
	ErrPolicyInvalid     = errors.New("WEB_ENDPOINT_POLICY_INVALID")
	ErrNotAllowed        = errors.New("WEB_ENDPOINT_NOT_ALLOWED")
	ErrBaseURLInvalid    = errors.New("WEB_ENDPOINT_INVALID")
	ErrRedirectForbidden = errors.New("WEB_ENDPOINT_REDIRECT_FORBIDDEN")
	ErrRequestFailed     = errors.New("WEB_ENDPOINT_REQUEST_FAILED")
)

// CheckBaseURL validates a saved provider base URL against the same immutable
// origin policy used by Do. It does no DNS lookup or network request. Dispatch
// must still pass through Do so resolved request URLs are checked again.
func (c *Client) CheckBaseURL(value string) error {
	if c == nil || len(c.origins) == 0 {
		return ErrPolicyInvalid
	}
	if value == "" || strings.ContainsAny(value, "?#") {
		return ErrBaseURLInvalid
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrBaseURLInvalid
	}
	origin, ok := normalizeURL(parsed)
	if !ok {
		return ErrBaseURLInvalid
	}
	if _, admitted := c.origins[origin]; !admitted {
		return ErrNotAllowed
	}
	return nil
}

// Client owns its immutable policy, native HTTP client, and private transport.
// Use New; a zero Client fails closed. It is safe for concurrent Do calls, but, as
// with net/http, callers must not mutate a request concurrently with its snapshot.
// Request bodies are streams and are not copied. Close closes idle connections,
// not in-flight requests or response bodies, which remain the caller's property.
type Client struct {
	origins   map[string]struct{}
	transport *http.Transport
	native    *http.Client
}

// New accepts only explicit HTTP(S) origins (an optional root slash is allowed).
// Wildcards, credentials (even empty userinfo), paths, query/fragment markers,
// ambiguous numeric/dotted hosts, percent hosts, and invalid ports fail closed.
// ASCII DNS names and canonical IP literals are supported; Unicode hostnames
// must be supplied as ASCII IDNA/punycode by the configuration owner.
func New(origins []string) (*Client, error) {
	if len(origins) == 0 {
		return nil, ErrPolicyInvalid
	}
	allowed := make(map[string]struct{}, len(origins))
	for _, value := range origins {
		value = strings.TrimSpace(value)
		parsed, err := url.Parse(value)
		if err != nil || strings.ContainsAny(value, "?#") || parsed == nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
			return nil, ErrPolicyInvalid
		}
		origin, ok := normalizeURL(parsed)
		if !ok {
			return nil, ErrPolicyInvalid
		}
		allowed[origin] = struct{}{}
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	// Dial through tcp4 for every egress connection. This host's container IPv6
	// link has no internet route, and the bridge DNS resolves some providers to
	// AAAA only, so a stock dual-stack dialer fails instantly before Happy
	// Eyeballs can try the A record. tcp4 keeps the DNS lookup to A records; the
	// committed origin set above is still revalidated on every send.
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", address)
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		ForceAttemptHTTP2:     true,
	}
	client := &Client{origins: allowed, transport: transport}
	client.native = &http.Client{
		Transport: &originTransport{client: client},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return ErrRedirectForbidden
		},
		// No overall timeout: streaming bodies retain their context lifetime.
	}
	return client, nil
}

// Do snapshots the entire request (including URL and headers), validates the
// fully resolved dispatch URL, and retains the original cancellation context.
// Host may only be empty or exactly the original URL authority, never a routing
// override. The native transport boundary revalidates another private snapshot.
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	if c == nil || c.native == nil || c.transport == nil || len(c.origins) == 0 {
		return nil, ErrPolicyInvalid
	}
	snapshot, err := c.snapshot(request)
	if err != nil {
		return nil, err
	}
	response, err := c.native.Do(snapshot)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		switch {
		case errors.Is(err, ErrRedirectForbidden):
			return nil, ErrRedirectForbidden
		case errors.Is(err, ErrNotAllowed):
			return nil, ErrNotAllowed
		case snapshot.Context().Err() != nil:
			return nil, snapshot.Context().Err()
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			return nil, ErrRequestFailed
		}
	}
	return response, nil
}

// Close releases only this client's idle connections. It is idempotent.
func (c *Client) Close() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// originTransport is deliberately private and cannot be supplied by callers.
// Reject redirects before http.Client parses Location (including malformed or
// credential-bearing Location values), and close rather than expose their body.
type originTransport struct {
	client *Client
}

func (t *originTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	snapshot, err := t.client.snapshot(request)
	if err != nil {
		return nil, err
	}
	response, err := t.client.transport.RoundTrip(snapshot)
	if err != nil {
		return nil, err // Do sanitizes all native transport errors.
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ErrRedirectForbidden
	}
	return response, nil
}

func (c *Client) snapshot(request *http.Request) (*http.Request, error) {
	if request == nil || request.URL == nil || request.RequestURI != "" {
		return nil, ErrNotAllowed
	}
	cloned := request.Clone(request.Context())
	if cloned.Host != "" && cloned.Host != cloned.URL.Host {
		return nil, ErrNotAllowed
	}
	origin, ok := normalizeURL(cloned.URL)
	if !ok {
		return nil, ErrNotAllowed
	}
	if _, allowed := c.origins[origin]; !allowed {
		return nil, ErrNotAllowed
	}
	// Route both the socket and HTTP Host using the same checked authority.
	cloned.Host = ""
	return cloned, nil
}

// normalizeURL mutates only a private URL snapshot. It retains the full path,
// query and fragment while normalizing scheme, host case, IP spelling and port.
func normalizeURL(value *url.URL) (string, bool) {
	if value == nil || value.User != nil || value.Opaque != "" || value.OmitHost {
		return "", false
	}
	scheme := strings.ToLower(value.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if value.RawPath != "" && value.EscapedPath() != value.RawPath {
		return "", false
	}
	if value.RawFragment != "" && value.EscapedFragment() != value.RawFragment {
		return "", false
	}
	// Do not silently discard malformed/contradictory URL struct fields. Parsing
	// the serialized full URL also rejects embedded control characters.
	reparsed, err := url.Parse(value.String())
	if err != nil || reparsed.User != nil || reparsed.Host != value.Host ||
		reparsed.Path != value.Path || reparsed.RawQuery != value.RawQuery ||
		reparsed.Fragment != value.Fragment || reparsed.ForceQuery != value.ForceQuery {
		return "", false
	}
	authority, ok := normalizeAuthority(scheme, value.Host)
	if !ok {
		return "", false
	}
	value.Scheme = scheme
	value.Host = authority
	return scheme + "://" + authority, true
}

func normalizeAuthority(scheme, authority string) (string, bool) {
	if authority == "" || strings.ContainsAny(authority, "/\\?#@% \t\r\n") {
		return "", false
	}
	host, port, hasPort := authority, "", false
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", false
		}
		suffix := host[end+1:]
		if suffix != "" {
			if !strings.HasPrefix(suffix, ":") {
				return "", false
			}
			port, hasPort = suffix[1:], true
		}
		address, err := netip.ParseAddr(host[1:end])
		if err != nil || !address.Is6() || address.Zone() != "" {
			return "", false
		}
		host = "[" + address.String() + "]"
	} else {
		if strings.Count(host, ":") > 1 {
			return "", false
		}
		if colon := strings.IndexByte(host, ':'); colon >= 0 {
			port, hasPort = host[colon+1:], true
			host = host[:colon]
		}
		host = strings.ToLower(host)
		if address, err := netip.ParseAddr(host); err == nil && address.Is4() {
			host = address.String()
		} else if !validDNSHost(host) {
			return "", false
		}
	}
	if hasPort {
		if !digits(port, false) {
			return "", false
		}
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		if (scheme == "http" && number == 80) || (scheme == "https" && number == 443) {
			return host, true
		}
		return host + ":" + strconv.FormatUint(number, 10), true
	}
	return host, true
}

func validDNSHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	// WHATWG interprets domains ending in an IPv4 number as IPv4, sometimes
	// accepting octal, hex or abbreviated forms. Require netip's unambiguous
	// dotted-quad form instead; do not treat those aliases as DNS names.
	last := labels[len(labels)-1]
	if digits(last, false) || (strings.HasPrefix(last, "0x") && (last == "0x" || digits(last[2:], true))) {
		return false
	}
	return true
}

func digits(value string, hexadecimal bool) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= '0' && char <= '9' {
			continue
		}
		if hexadecimal && char >= 'a' && char <= 'f' {
			continue
		}
		return false
	}
	return true
}
