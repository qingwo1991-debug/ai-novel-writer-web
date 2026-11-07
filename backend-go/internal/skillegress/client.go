// Package skillegress is the second, narrow outbound client of the production
// lane. It fetches nothing but public GitHub writing-skill content and is the
// dispatcher the promptskills remote ports dial through
// (internal/promptskills/remote.go GitHubDispatcher).
//
// The origin set is fixed at two HTTPS origins and is never configurable:
//
//	https://api.github.com
//	https://raw.githubusercontent.com
//
// A request argument cannot widen it: no operation argument, header, committed
// value or environment variable reaches New, and the two origins are package
// constants. The promptskills dispatcher path already parses every user-supplied
// source URL with ParseGitHubURL and reconstructs the two endpoints from its
// safe parts before this client is asked to dial. This client independently
// re-checks the endpoint origin and scheme, refuses any 3xx before its Location
// is read, bounds the body and the whole fetch time, and never follows a
// redirect. It shares no state with the model egress (internal/egress), whose
// origin set is the author's committed model allowlist.
package skillegress

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// The exact two hosts this client may dial, the origins
	// https://api.github.com and https://raw.githubusercontent.com. They are the
	// only authority the endpoint check below consults, and no other host can
	// ever be added. A downgrade to http, a port, a credential or a foreign host
	// is refused before any dial.
	apiHost = "api.github.com"
	rawHost = "raw.githubusercontent.com"

	// maxFetchBytes mirrors promptskills.MaxSkillBytes. The dispatcher passes
	// that same bound, and this client refuses a larger cap so a caller cannot
	// ask it to read an unbounded body.
	maxFetchBytes = 64 * 1024

	// fetchTimeout bounds one fetch end to end, body read included. It matches
	// the desktop's own 10 second budget (electron/controllers/app-data-controller.ts).
	fetchTimeout = 10 * time.Second
)

// The refusal vocabulary is symbolic and fixed: a rejected URL or a native
// transport error never carries the URL, which http.Client would otherwise
// expose inside *url.Error.
var (
	// ErrNotAllowed is a foreign host, a non-https scheme, a port, a credential,
	// a query/fragment, or a malformed URL - refused before any dial.
	ErrNotAllowed = errors.New("SKILL_EGRESS_NOT_ALLOWED")
	// ErrRedirect is any 3xx answer, refused before the Location is read.
	ErrRedirect = errors.New("SKILL_EGRESS_REDIRECT_FORBIDDEN")
	// ErrTooLarge is a body, or a requested cap, over the fixed bound.
	ErrTooLarge = errors.New("SKILL_EGRESS_BODY_TOO_LARGE")
	// ErrFetchFailed is a transport failure whose own text is not exposed.
	ErrFetchFailed = errors.New("SKILL_EGRESS_FETCH_FAILED")
	// ErrUnavailable is a nil or unbuilt client.
	ErrUnavailable = errors.New("SKILL_EGRESS_UNAVAILABLE")
)

// Client owns its immutable two-origin policy and its native HTTP client. It is
// safe for concurrent FetchGitHub calls. Use New, or NewWithTransport only to
// stub the socket in a test; both keep the same origin, redirect and size
// policy. The origin check below is the same check the native transport path
// enforces, so a request argument can never re-route a fetch to another host.
type Client struct {
	native *http.Client
}

// New builds the production client over the real network.
func New() (*Client, error) {
	return newClient(nil)
}

// NewWithTransport builds a client whose socket I/O is the given round tripper.
// Origins, scheme, redirect refusal, size and timeout bounds are unchanged: the
// round tripper is the client's own network seam, present so a test can stub the
// socket without weakening any admission. A nil round tripper is refused.
func NewWithTransport(transport http.RoundTripper) (*Client, error) {
	if transport == nil {
		return nil, ErrUnavailable
	}
	return newClient(transport)
}

func newClient(transport http.RoundTripper) (*Client, error) {
	if transport == nil {
		transport = &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: fetchTimeout,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			ForceAttemptHTTP2:     true,
		}
	}
	native := &http.Client{
		Transport: &originTransport{roundTrip: transport},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			// originTransport refuses a 3xx first; this is the second wall in
			// case a future transport ever passes one through.
			return ErrRedirect
		},
		Timeout: fetchTimeout,
	}
	return &Client{native: native}, nil
}

// FetchGitHub fetches one already-reconstructed GitHub endpoint and returns its
// status and body, bounded to maxBytes. It is the concrete implementation of
// promptskills.GitHubDispatcher.FetchGitHub: the same signature, and the same
// contract - ParseGitHubURL has already run on the source URL upstream
// (internal/promptskills/remote.go), and this client re-verifies the endpoint
// origin before any socket. A foreign host, a redirect, an oversize body or a
// failed fetch returns one of the symbolic errors above, never the URL.
func (c *Client) FetchGitHub(ctx context.Context, rawURL string, maxBytes int) (int, []byte, error) {
	if c == nil || c.native == nil {
		return 0, nil, ErrUnavailable
	}
	if maxBytes < 0 || maxBytes > maxFetchBytes {
		return 0, nil, ErrTooLarge
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !admittedURL(rawURL) {
		return 0, nil, ErrNotAllowed
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, ErrNotAllowed
	}
	request.Header.Set("Accept", "application/vnd.github+json, text/plain")
	request.Header.Set("User-Agent", "AI-Novel-Writer")
	response, err := c.native.Do(request)
	if err != nil {
		return 0, nil, mapFetchError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return response.StatusCode, nil, ErrRedirect
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return response.StatusCode, nil, mapFetchError(err)
	}
	if len(body) > maxBytes {
		return response.StatusCode, nil, ErrTooLarge
	}
	return response.StatusCode, body, nil
}

// admittedURL accepts exactly an HTTPS endpoint on one of the two fixed hosts:
// no credential, no port, no opaque, query, fragment or force-query marker, and
// no contradictory RawPath. It performs no DNS lookup and starts no request.
func admittedURL(rawURL string) bool {
	if rawURL == "" || len(rawURL) > 2048 || strings.ContainsRune(rawURL, '\x00') {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil {
		return false
	}
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	if parsed.Host == "" || parsed.Port() != "" {
		return false
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	if parsed.RawPath != "" && parsed.EscapedPath() != parsed.RawPath {
		return false
	}
	host := strings.ToLower(parsed.Host)
	return host == apiHost || host == rawHost
}

// originTransport is deliberately private: callers cannot supply it. It refuses
// a 3xx before http.Client parses the Location, and closes rather than exposes
// the redirect body, so a second host is never dialed.
type originTransport struct {
	roundTrip http.RoundTripper
}

func (t *originTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.roundTrip.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ErrRedirect
	}
	return response, nil
}

// mapFetchError collapses every native transport error to a symbolic code. A
// native error is usually a *url.Error carrying the full request URL, so its own
// text is never surfaced.
func mapFetchError(err error) error {
	switch {
	case errors.Is(err, ErrRedirect):
		return ErrRedirect
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrFetchFailed
	}
}
