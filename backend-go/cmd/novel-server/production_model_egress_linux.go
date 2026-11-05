//go:build linux

package main

import (
	"errors"
	"net/url"
	"sort"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

// The production lane has no administrator flag, so the exact HTTP(S) origins a
// future generation executor may dial are derived from the author's own
// committed global generation, once, at startup. Two sources are read in this
// order:
//
//  1. the optional modelOrigins array of the committed config.json - the same
//     document llm:get-default-model reads. When present it is authoritative, so
//     a model profile whose endpoint falls outside it stays refused.
//  2. otherwise the baseUrl of every committed model profile in models.json,
//     reduced to its origin.
//
// Nothing else is consulted: no environment, no flag, no request field and no
// default list. An absent, unreadable or malformed source yields a symbolic
// refusal and no policy at all - never a policy that admits arbitrary hosts.
const (
	// productionMaxOriginLength bounds one committed origin (or model endpoint)
	// string before it is parsed.
	productionMaxOriginLength = 2048
)

// Symbolic refusals. They are fixed codes: a rejected value is never formatted
// into one, because a malformed committed endpoint may itself carry a credential
// (https://user:secret@host).
var (
	errProductionModelEgressUnconfigured = errors.New("PRODUCTION_MODEL_EGRESS_UNCONFIGURED")
	errProductionModelEgressInvalid      = errors.New("PRODUCTION_MODEL_EGRESS_INVALID")
	errProductionModelEgressUnavailable  = errors.New("PRODUCTION_MODEL_EGRESS_UNAVAILABLE")
)

// productionModelEgress is the immutable model-egress boundary derived from the
// committed config. origins is provenance only - exactly the committed values,
// deduplicated and sorted. The sole authority is policy, whose private origin set
// egress.Client.Do revalidates at dispatch time; that is also why a future
// executor must dial through policy and not through a bare http.Client.
type productionModelEgress struct {
	origins []string
	policy  *egress.Client
}

// productionModelEgressPlan builds the policy, or refuses with one of the three
// symbolic errors above. It performs no network call and constructs no provider
// client: validating an origin is pure parsing, and egress.New only assembles its
// private transport.
func productionModelEgressPlan(config *globalconfig.Service) (*productionModelEgress, error) {
	if config == nil {
		return nil, errProductionModelEgressUnavailable
	}
	declared, present, err := config.ModelEgressOrigins()
	if err != nil {
		// A config that cannot be read is not an absent one; either way no policy.
		return nil, errProductionModelEgressUnavailable
	}
	var candidates []string
	if present {
		candidates, err = productionDeclaredOrigins(declared)
	} else {
		candidates, err = productionCommittedModelOrigins(config)
	}
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		// The committed config names no origin. Refuse; do not invent one and do
		// not fall back to an unrestricted policy.
		return nil, errProductionModelEgressUnconfigured
	}
	origins := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if !productionOriginValid(candidate) {
			return nil, errProductionModelEgressInvalid
		}
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		origins = append(origins, candidate)
	}
	sort.Strings(origins)
	policy, err := egress.New(origins)
	if err != nil {
		// egress.New is the authoritative validator; its errors are symbolic.
		return nil, errProductionModelEgressInvalid
	}
	return &productionModelEgress{origins: origins, policy: policy}, nil
}

// productionDeclaredOrigins projects the committed allowlist. A value that is not
// an array of strings is malformed, not an empty list: an author who wrote
// "modelOrigins": "https://host" must not silently get an unlimited policy.
func productionDeclaredOrigins(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, errProductionModelEgressInvalid
	}
	origins := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, errProductionModelEgressInvalid
		}
		origins = append(origins, text)
	}
	return origins, nil
}

// productionCommittedModelOrigins derives the allowlist from the endpoints the
// author already committed. Only baseUrl is read: the record's apiKey and every
// other credential field stay in the private store and are never copied,
// formatted, returned or logged here.
func productionCommittedModelOrigins(config *globalconfig.Service) ([]string, error) {
	records, err := config.ListPrivateModels()
	if err != nil {
		return nil, errProductionModelEgressUnavailable
	}
	origins := make([]string, 0, len(records))
	for _, record := range records {
		base, ok := record["baseUrl"].(string)
		if !ok {
			// A profile without a committed endpoint cannot be dispatched to, and
			// must not be read as "any origin".
			return nil, errProductionModelEgressInvalid
		}
		origin, ok := productionEndpointOrigin(base)
		if !ok {
			return nil, errProductionModelEgressInvalid
		}
		origins = append(origins, origin)
	}
	return origins, nil
}

// productionOriginValid accepts exactly one absolute HTTP(S) origin: a scheme, a
// host and an optional port, plus at most a root slash. Wildcards, credentials
// (even an empty userinfo), paths, query/fragment markers, control characters,
// other schemes and any non-absolute value fail closed.
func productionOriginValid(value string) bool {
	if len(value) == 0 || len(value) > productionMaxOriginLength || strings.TrimSpace(value) != value {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n\\*") || strings.ContainsRune(value, '\x00') {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return false
	}
	if parsed.User != nil || parsed.Host == "" {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	if parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

// productionEndpointOrigin reduces one committed model base URL to its dispatch
// origin. The path ("/v1/chat/completions") is part of the endpoint, not the
// origin, so it is dropped; a userinfo, query or fragment is refused instead of
// being dropped, because dropping it would hide a credential or a routing
// override the author committed.
func productionEndpointOrigin(value string) (string, bool) {
	if len(value) == 0 || len(value) > productionMaxOriginLength || strings.TrimSpace(value) != value {
		return "", false
	}
	if strings.ContainsAny(value, " \t\r\n\\*") || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Opaque != "" {
		return "", false
	}
	if parsed.User != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	return scheme + "://" + parsed.Host, true
}
