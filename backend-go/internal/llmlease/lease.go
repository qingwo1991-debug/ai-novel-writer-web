// Package llmlease owns the browser model-execution lease that the renderer's
// frozen llm:* family resolves through.
//
// A lease is the only thing that lets one generation run hold provider
// authority. It is bound to the browser tab that opened it (one cookie session
// plus one X-Client-Id), to the frozen model profile it was opened over, to the
// project it is first used for, and to itself as the identity of the run it
// names; it expires, and it is closed by llm:close-execution-lease or dropped by
// the failure path that finds it unusable. A request that presents no valid
// lease for this tab refuses by name instead of falling back to a profile
// lookup.
//
// The package owns no network client, no database handle and no author content.
// It stores one detached profile snapshot per lease, including the credential
// bytes that profile already carried in the author's own models.json, because
// the execute path resolves the key at request time and must not read the file
// twice inside one run. Nothing that leaves here is a credential: the receipt is
// generationowner.Receipt, the secret-free projection the renderer stores as
// ModelExecutionLeaseReceipt (src/shared/ipc-channels.ts:779-790: leaseId,
// modelId, provider, protocol, modelName, modelRevision, endpointFingerprint,
// capabilityEvidence, createdAt, expiresAt), projected with the renderer's own
// sha256(JSON.stringify(...)) field order. The wire type is reused rather than
// restated so the Go answer cannot drift from the renderer's contract.
//
// It deliberately does not reuse generationowner.Registry or
// generationowner.CreateReceipt. That registry is the durable generation
// owner's admission, and its catalog (internal/generationowner/lease.go:85-130)
// refuses every official preset host except api.openai.com/gpt-4.1. The
// renderer's own lease registry (electron/services/model-execution-lease.ts:
// 100-130) applies no such catalog - it requires a positive output bound and
// nothing else - so a settings screen that offers a DeepSeek, Gemini or
// SiliconFlow preset would be refused by a lease it can see. The evidence this
// package emits therefore never claims verified provider capacities: it says
// "unknown", "legacy-profile" or "user-operational-cap", which is exactly what
// a server without a provider-preset catalog can honestly attest.
package llmlease

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
)

const (
	// DefaultTTL mirrors DEFAULT_MODEL_EXECUTION_LEASE_TTL_MS
	// (electron/services/model-execution-lease.ts:13). A run that outlives its
	// lease must be refused, never silently renewed.
	DefaultTTL = 4 * time.Hour
	// DefaultMaxLeases bounds one process's registry so a sequence of begin calls
	// cannot grow memory without limit. Leases are tiny (one profile snapshot).
	DefaultMaxLeases = 256
	// safeInteger is JavaScript's Number.MAX_SAFE_INTEGER: a receipt may not carry
	// a bound the renderer cannot represent.
	safeInteger int64 = 9007199254740991
)

// Symbolic refusals. The first two are the renderer's own lease codes
// (electron/services/model-execution-lease.ts:24-30); the rest stay inside this
// server and are never formatted into an answer.
var (
	ErrInvalid       = errors.New("MODEL_EXECUTION_LEASE_INVALID")
	ErrExpired       = errors.New("MODEL_EXECUTION_LEASE_EXPIRED")
	ErrModelNotFound = errors.New("MODEL_NOT_FOUND")
	ErrModelInvalid  = errors.New("MODEL_PROFILE_INVALID")
	ErrEndpoint      = errors.New("MODEL_ENDPOINT_INVALID")
	ErrOutputLimit   = errors.New("MODEL_OUTPUT_LIMIT_INVALID")
	ErrChanged       = errors.New("GENERATION_MODEL_CHANGED")
	ErrLimit         = errors.New("MODEL_EXECUTION_LEASE_LIMIT")
	ErrUnavailable   = errors.New("MODEL_EXECUTION_CONFIG_REQUIRED")
)

// Owner is the browser tab a lease belongs to. Both halves are captured by the
// authenticated transport (internal/transport/server.go Caller) and never
// decoded from a request body, so one tab cannot reach another's lease.
type Owner struct {
	Session string
	Tab     string
}

func (o Owner) valid() bool { return o.Session != "" && o.Tab != "" }

// Options are explicit server capabilities. LoadModel must read the author's own
// committed configuration and must never consult a request field.
type Options struct {
	LoadModel func(context.Context, string) (generationowner.Profile, bool, error)
	TTL       time.Duration
	MaxLeases int
	Now       func() time.Time
	NewID     func() (string, error)
}

// Snapshot is one resolved lease: the frozen private profile the execute path
// dials with, and the secret-free receipt the browser already holds. Profile is
// the private half and must never be projected to a browser.
type Snapshot struct {
	Profile generationowner.Profile
	Receipt generationowner.Receipt
}

type record struct {
	receipt generationowner.Receipt
	profile generationowner.Profile
	owner   Owner
	project string
	// hash is the credential fingerprint this lease was opened with. It is never
	// the credential: it only proves the key did not change under the run.
	hash string
}

// Registry is the in-memory lease store. It is safe for concurrent use.
type Registry struct {
	mu        sync.Mutex
	loadModel func(context.Context, string) (generationowner.Profile, bool, error)
	ttl       time.Duration
	maxLeases int
	now       func() time.Time
	newID     func() (string, error)
	records   map[string]*record
}

// New validates the injected capabilities. A registry without a model source
// cannot resolve anything, so it refuses to be built.
func New(options Options) (*Registry, error) {
	if options.LoadModel == nil {
		return nil, ErrUnavailable
	}
	registry := &Registry{
		loadModel: options.LoadModel,
		ttl:       options.TTL,
		maxLeases: options.MaxLeases,
		now:       options.Now,
		newID:     options.NewID,
		records:   make(map[string]*record),
	}
	if registry.ttl <= 0 {
		registry.ttl = DefaultTTL
	}
	if registry.maxLeases <= 0 {
		registry.maxLeases = DefaultMaxLeases
	}
	if registry.now == nil {
		registry.now = time.Now
	}
	if registry.newID == nil {
		registry.newID = newLeaseID
	}
	return registry, nil
}

// Begin freezes one model profile into a new lease and returns its receipt. The
// profile is read from the author's committed configuration here, once, and the
// receipt is the only thing that travels back to the browser.
func (r *Registry) Begin(ctx context.Context, owner Owner, modelID string) (generationowner.Receipt, error) {
	if r == nil || ctx == nil || !owner.valid() || modelID == "" {
		return generationowner.Receipt{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return generationowner.Receipt{}, err
	}
	profile, found, err := r.loadModel(ctx, modelID)
	if err != nil {
		return generationowner.Receipt{}, ErrUnavailable
	}
	if !found {
		return generationowner.Receipt{}, ErrModelNotFound
	}
	leaseID, err := r.newID()
	if err != nil || leaseID == "" {
		return generationowner.Receipt{}, ErrUnavailable
	}
	now := r.now()
	receipt, err := createReceipt(profile, leaseID, now, r.ttl)
	if err != nil {
		return generationowner.Receipt{}, err
	}
	entry := &record{
		receipt: receipt,
		profile: cloneProfile(profile),
		owner:   owner,
		hash:    fingerprint(profile.EncryptedCredential),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	if len(r.records) >= r.maxLeases {
		return generationowner.Receipt{}, ErrLimit
	}
	r.records[receipt.LeaseID] = entry
	return receipt, nil
}

// Resolve returns the frozen snapshot for one lease, or refuses by name. It is
// the only way a generation request reaches a provider: no lease, another tab's
// lease, an expired lease, a model that changed on disk since the lease was
// opened, or a second project all refuse here, before any request is built.
//
// projectID may be empty (a caller that names no project, which the renderer
// allows for llm:generate): the first project that names this lease binds it and
// every later request must name the same one.
func (r *Registry) Resolve(ctx context.Context, owner Owner, leaseID, projectID string) (Snapshot, error) {
	if r == nil || ctx == nil || !owner.valid() || leaseID == "" {
		return Snapshot{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	entry, found := r.records[leaseID]
	if !found {
		r.mu.Unlock()
		return Snapshot{}, ErrInvalid
	}
	if entry.owner != owner {
		r.mu.Unlock()
		return Snapshot{}, ErrInvalid
	}
	if r.now().UnixMilli() >= entry.receipt.ExpiresAt {
		delete(r.records, leaseID)
		r.mu.Unlock()
		return Snapshot{}, ErrExpired
	}
	frozen := Snapshot{Profile: cloneProfile(entry.profile), Receipt: cloneReceipt(entry.receipt)}
	hash := entry.hash
	r.mu.Unlock()

	// The credential and the profile are re-read from the author's own
	// configuration at request time; a file that changed since begin (an edited
	// endpoint, a rotated key, a deleted profile) invalidates the lease instead of
	// dispatching a run nobody can reproduce. generationowner.lease.go:150-156
	// applies the same rule to the durable owner's leases.
	live, present, err := r.loadModel(ctx, frozen.Receipt.ModelID)
	if err != nil || !present || fingerprint(live.EncryptedCredential) != hash {
		r.drop(owner, leaseID)
		return Snapshot{}, ErrChanged
	}
	revision, err := modelRevision(live)
	if err != nil || revision != frozen.Receipt.ModelRevision {
		r.drop(owner, leaseID)
		return Snapshot{}, ErrChanged
	}

	if projectID != "" {
		r.mu.Lock()
		latest, ok := r.records[leaseID]
		if !ok {
			r.mu.Unlock()
			return Snapshot{}, ErrInvalid
		}
		if latest.project == "" {
			latest.project = projectID
		} else if latest.project != projectID {
			r.mu.Unlock()
			return Snapshot{}, ErrInvalid
		}
		r.mu.Unlock()
	}
	return frozen, nil
}

// Close releases one lease. Only the tab that opened it may close it. It reports
// false for an unknown, expired or foreign lease, which is what the renderer's
// closed-lease tombstone treats as an already-closed lease.
func (r *Registry) Close(owner Owner, leaseID string) bool {
	if r == nil || !owner.valid() || leaseID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, found := r.records[leaseID]
	if !found || entry.owner != owner {
		return false
	}
	delete(r.records, leaseID)
	return true
}

// Outstanding reports how many leases this registry still holds. It exists so a
// test can prove a close or a failure really dropped the record.
func (r *Registry) Outstanding() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func (r *Registry) drop(owner Owner, leaseID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, found := r.records[leaseID]; found && entry.owner == owner {
		delete(r.records, leaseID)
	}
}

func (r *Registry) pruneLocked(now time.Time) {
	for id, entry := range r.records {
		if now.UnixMilli() >= entry.receipt.ExpiresAt {
			delete(r.records, id)
		}
	}
}

// newLeaseID is a random UUIDv4, the same shape randomUUID produces in the
// renderer's registry (electron/services/model-execution-lease.ts:157). It is
// never derived from a request field.
func newLeaseID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 15) | 64
	raw[8] = (raw[8] & 63) | 128
	text := hex.EncodeToString(raw[:])
	return text[:8] + "-" + text[8:12] + "-" + text[12:16] + "-" + text[16:20] + "-" + text[20:], nil
}

// createReceipt builds the secret-free receipt for one frozen profile. Every
// hash is the renderer's own sha256 over the same field order
// (electron/services/model-execution-lease.ts:96-118).
func createReceipt(profile generationowner.Profile, leaseID string, now time.Time, ttl time.Duration) (generationowner.Receipt, error) {
	endpoint, err := normalizedEndpoint(profile.BaseURL)
	if err != nil {
		return generationowner.Receipt{}, err
	}
	evidence, err := capabilityEvidence(profile, endpoint)
	if err != nil {
		return generationowner.Receipt{}, err
	}
	revision, err := modelRevision(profile)
	if err != nil {
		return generationowner.Receipt{}, err
	}
	created := now.UnixMilli()
	return generationowner.Receipt{
		LeaseID:             leaseID,
		ModelID:             profile.ID,
		Provider:            profile.Provider,
		Protocol:            profile.Protocol,
		ModelName:           profile.ModelName,
		ModelRevision:       revision,
		EndpointFingerprint: endpointFingerprint(profile),
		CapabilityEvidence:  evidence,
		CreatedAt:           created,
		ExpiresAt:           created + ttl.Milliseconds(),
	}, nil
}

// normalizedEndpoint is normalizeEndpoint (electron/services/model-execution-
// lease.ts:60-71): scheme, host and path only, with trailing slashes removed. A
// credential, query or fragment in a committed base URL is refused rather than
// folded into a fingerprint, which is also what the committed egress policy does
// (internal/egress/client.go:40-45).
func normalizedEndpoint(baseURL string) (string, error) {
	value := strings.TrimSpace(baseURL)
	if value == "" || strings.Contains(value, "@") {
		return "", ErrEndpoint
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || parsed.Host == "" {
		return "", ErrEndpoint
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", ErrEndpoint
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", ErrEndpoint
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

type endpointSubject struct {
	Provider string `json:"provider"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
}

type modelSubject struct {
	Provider  string `json:"provider"`
	Protocol  string `json:"protocol"`
	Endpoint  string `json:"endpoint"`
	ModelName string `json:"modelName"`
}

type revisionSubject struct {
	ID                string          `json:"id"`
	Provider          string          `json:"provider"`
	Protocol          string          `json:"protocol"`
	Endpoint          string          `json:"endpoint"`
	ModelName         string          `json:"modelName"`
	Temperature       float64         `json:"temperature"`
	MaxTokens         int64           `json:"maxTokens"`
	Capabilities      json.RawMessage `json:"capabilities"`
	ReasoningOverride string          `json:"reasoningOverride"`
	ReasoningMapping  json.RawMessage `json:"reasoningMapping,omitempty"`
	Purposes          []string        `json:"purposes"`
	EmbeddingOptions  json.RawMessage `json:"embeddingOptions"`
}

func endpointFingerprint(profile generationowner.Profile) string {
	endpoint, err := normalizedEndpoint(profile.BaseURL)
	if err != nil {
		return ""
	}
	return hashJSON(endpointSubject{Provider: profile.Provider, Protocol: profile.Protocol, Endpoint: endpoint})
}

// modelRevision is the fingerprint of the whole frozen profile, so an edit that
// does not move the endpoint (a temperature, an output cap, a purpose) still
// invalidates the lease.
func modelRevision(profile generationowner.Profile) (string, error) {
	endpoint, err := normalizedEndpoint(profile.BaseURL)
	if err != nil {
		return "", err
	}
	purposes := profile.Purposes
	if purposes == nil {
		purposes = []string{}
	}
	override := profile.ReasoningOverride
	if override == "" {
		override = "auto"
	}
	value := hashJSON(revisionSubject{
		ID:                profile.ID,
		Provider:          profile.Provider,
		Protocol:          profile.Protocol,
		Endpoint:          endpoint,
		ModelName:         profile.ModelName,
		Temperature:       profile.Temperature,
		MaxTokens:         profile.MaxTokens,
		Capabilities:      rawOrNull(profile.Capabilities),
		ReasoningOverride: override,
		ReasoningMapping:  profile.ReasoningMapping,
		Purposes:          purposes,
		EmbeddingOptions:  rawOrNull(profile.EmbeddingOptions),
	})
	if value == "" {
		return "", ErrModelInvalid
	}
	return value, nil
}

// capabilityEvidence mirrors resolveModelExecutionCapabilityEvidence
// (electron/services/model-execution-lease.ts:80-130) with no provider-preset
// catalog: a user-declared context window and output cap, or the legacy
// maxTokens, are the only bounds this server can attest.
func capabilityEvidence(profile generationowner.Profile, endpoint string) (generationowner.Evidence, error) {
	hints := struct {
		Context *int64 `json:"contextWindowTokens"`
		Output  *int64 `json:"maxOutputTokens"`
	}{}
	if raw := rawOrNull(profile.Capabilities); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &hints); err != nil {
			return generationowner.Evidence{}, ErrModelInvalid
		}
	}
	explicitContext := bounded(hints.Context)
	explicitOutput := bounded(hints.Output)
	legacyOutput := int64(0)
	if profile.MaxTokens > 0 && profile.MaxTokens <= safeInteger {
		legacyOutput = profile.MaxTokens
	}
	operational := explicitOutput
	if legacyOutput > 0 && (operational == 0 || legacyOutput < operational) {
		operational = legacyOutput
	}
	output := operational
	if explicitContext > 0 && (output == 0 || output > explicitContext) {
		output = explicitContext
	}
	if output <= 0 {
		return generationowner.Evidence{}, ErrOutputLimit
	}
	contextSource := "unknown"
	if explicitContext > 0 {
		contextSource = "user-operational-cap"
	}
	outputSource := "legacy-profile"
	if explicitOutput > 0 {
		outputSource = "user-operational-cap"
	}
	subject := hashJSON(modelSubject{
		Provider:  profile.Provider,
		Protocol:  profile.Protocol,
		Endpoint:  endpoint,
		ModelName: profile.ModelName,
	})
	if subject == "" {
		return generationowner.Evidence{}, ErrModelInvalid
	}
	evidence := generationowner.Evidence{
		Source:             generationowner.EvidenceSource{Context: contextSource, Output: outputSource, Features: "unknown"},
		SubjectFingerprint: subject,
		Output:             output,
	}
	if explicitContext > 0 {
		value := explicitContext
		evidence.Context = &value
	}
	return evidence, nil
}

func bounded(value *int64) int64 {
	if value == nil || *value <= 0 || *value > safeInteger {
		return 0
	}
	return *value
}

func rawOrNull(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return value
}

// hashJSON is sha256 over the compact JSON of one value, with the renderer's
// escaping rules: no HTML escaping and literal U+2028/U+2029, exactly like
// JSON.stringify (internal/generationowner/lease.go:80 applies the same rule).
func hashJSON(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	raw := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	raw = bytes.ReplaceAll(raw, []byte(`\u2028`), []byte("\u2028"))
	raw = bytes.ReplaceAll(raw, []byte(`\u2029`), []byte("\u2029"))
	return hex.EncodeToString(sum256(raw))
}

func fingerprint(value []byte) string { return hex.EncodeToString(sum256(value)) }

func sum256(raw []byte) []byte {
	sum := sha256.Sum256(raw)
	return sum[:]
}

// cloneProfile detaches one profile, credential bytes included. Profile's
// credential field is json:"-" so a JSON round trip cannot carry it.
func cloneProfile(profile generationowner.Profile) generationowner.Profile {
	out := profile
	out.EncryptedCredential = append([]byte(nil), profile.EncryptedCredential...)
	out.Purposes = append([]string(nil), profile.Purposes...)
	out.Capabilities = append(json.RawMessage(nil), profile.Capabilities...)
	out.ReasoningMapping = append(json.RawMessage(nil), profile.ReasoningMapping...)
	out.EmbeddingOptions = append(json.RawMessage(nil), profile.EmbeddingOptions...)
	return out
}

func cloneReceipt(receipt generationowner.Receipt) generationowner.Receipt {
	out := receipt
	if receipt.CapabilityEvidence.Context != nil {
		value := *receipt.CapabilityEvidence.Context
		out.CapabilityEvidence.Context = &value
	}
	return out
}
