package mcphost

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)

// Policy is the committed-origin boundary every MCP request passes. It owns no
// client of its own: the only thing it can dispatch through is the egress client
// the production lane derived from the author's committed configuration
// (productionOpen.ModelEgress), which keeps a private origin set, refuses
// redirects before a Location header is parsed, ignores the environment proxy and
// revalidates the fully resolved URL at dispatch time.
//
// A nil Policy, or one built over a nil client, refuses every endpoint rather than
// falling back to an ambient client: "no committed policy" is a refusal, not a
// default allow.
type Policy struct {
	client *egress.Client
}

// NewPolicy wraps the lane's committed egress client. A nil client is accepted so
// a lane whose committed config names no origin can still load a configuration
// and answer honest statuses; it simply cannot connect anything.
func NewPolicy(client *egress.Client) *Policy {
	return &Policy{client: client}
}

// Admit validates one configured endpoint and answers the URL this transport may
// dial. It performs no DNS lookup and no network request: the origin question is
// answered by the policy's own pure check, and the same policy revalidates the
// request URL again when a call is actually made.
//
// The shape checks in front of that are deliberately stricter than a URL parser:
// an endpoint with credentials, a query, a fragment or another scheme is refused
// here, because those are exactly the parts that could carry a credential this
// lane has promised never to publish, and none of them is part of an MCP
// endpoint's identity.
func (p *Policy) Admit(endpoint string) (*url.URL, error) {
	if p == nil || p.client == nil {
		return nil, ErrPolicyUnavailable
	}
	if endpoint == "" || len(endpoint) > MaxServerURLLength {
		return nil, ErrServerURLInvalid
	}
	if !utf8.ValidString(endpoint) || strings.ContainsAny(endpoint, " \t\r\n\\") || strings.ContainsRune(endpoint, '\x00') {
		return nil, ErrServerURLInvalid
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || parsed.Opaque != "" || parsed.Host == "" {
		return nil, ErrServerURLInvalid
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, ErrServerURLInvalid
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return nil, ErrServerURLInvalid
	}
	// The committed policy's own answer, returned unchanged: ErrNotAllowed for an
	// origin it does not admit, ErrBaseURLInvalid for a value it rejects,
	// ErrPolicyInvalid for a policy it cannot trust. Code maps each of them.
	if err := p.client.CheckBaseURL(endpoint); err != nil {
		return nil, err
	}
	return parsed, nil
}

// Do dispatches one already-built request through the committed policy. The
// policy's own refusal comes back unchanged; it is symbolic and never carries the
// URL.
func (p *Policy) Do(request *http.Request) (*http.Response, error) {
	if p == nil || p.client == nil {
		return nil, ErrPolicyUnavailable
	}
	return p.client.Do(request)
}
