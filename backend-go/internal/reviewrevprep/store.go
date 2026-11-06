// Package reviewrevprep owns the in-process handoff between the
// `review-revision:prepare` producer and the `generation:begin` run admission.
//
// The freeze producer (internal/store/review_revision_context.go, Capture ->
// FrozenReviewRevisionContext) can already emit the exact frozen bytes and
// their sha256, but the wire between prepare and begin carries only an opaque
// id. The value therefore has to stay in this process under that id until the
// admission resolves it. This package is the registry: a bounded, expiring,
// caller-bound map from handle id to frozen review-revision context.
//
// # Reference
//
// electron/services/import-inspection-store.ts is the closest sibling and the
// pattern followed here. create mints an id and binds it to the caller that
// asked (webContentsId, :126-135), every entry carries expiresAt (:134),
// removeExpired runs on every mutation (:59, :70, :154, :162, :179, :185-188),
// peek does not consume (:161-170), and consume deletes exactly once (:153-159).
//
// internal/importsource/inspection.go is the existing Go port of that pattern
// and fixes the conventions used here: a constructor taking a server key plus
// StoreOptions, Inspect/Peek/Consume/RevokeCaller/ActiveCount, keyed digests,
// an injected clock, IMPORT_*-shaped refusal codes, and a revision counter that
// invalidates a caller's outstanding handles when it is revoked (:104-105, :137,
// :170).
//
// # Bearer-like handles
//
// import-inspection-store.ts enforces three of the four properties a handle
// needs, and this package keeps all three:
//
//   - unguessable: the id is a random UUID from crypto/rand (:125), never
//     derived from content, and this package adds nothing to it;
//   - it encodes neither the project path nor the caller: randomUUID is pure
//     entropy, and no project id, caller, generation or hash is mixed in;
//   - it is unusable by another caller: peek and consume require the stored
//     webContentsId (:156, :165), which is the Binding equality check at
//     internal/importsource/inspection.go:155 in Go; here the whole Binding
//     must match.
//
// It enforces no generation binding, and that is what this package adds. The
// reference compares a purpose at most (:165) and has no generation concept, so
// a renderer that switches runs keeps the same webContentsId and could replay a
// handle into the wrong run. Binding.Generation closes that hole: see the
// Binding comment at internal/importsource/inspection.go:12-19, which makes the
// same point for import receipts.
//
// # What this package deliberately does not do
//
// It opens no database, parses no JSON, validates no project lease, and does not
// depend on internal/store. It stores and returns the frozen Text together with
// its ContextHash, and lets the consumer re-parse the bytes: the producer
// guarantees plotJSON reproduces Text byte for byte
// (internal/store/review_revision_context.go:19-24, :219-221), so re-parsing is
// lossless and this package never hands out a mutable alias of a frozen value.
// Consume is not an admission: the consumer still needs the project lease, the
// generation-ledger write and the effect receipt, exactly as
// internal/importsource/inspection.go:161-164 says for import.
package reviewrevprep

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bounds. defaultTTL is IMPORT_INSPECTION_TTL_MS (import-inspection-store.ts:11)
// and maxTTL keeps a forgotten handle from pinning frozen text for longer than
// the sibling allows (inspection.go:84 rejects anything above ten minutes).
// maxActive and maxPerCaller cap entry counts the way inspection.go:140 does,
// maxTextBytes caps retained frozen bytes the way maxAggregateBytes does
// (inspection-store.ts:119-121), maxIdentityBytes/maxStatusBytes/maxHandleBytes
// bound the strings this package echoes back, handleDigestDomain separates the
// entry MAC from any other use of the server key, maxSafeInteger is the
// JavaScript safe-integer bound inspection.go applies to numeric fields, and
// revocationGrace (in milliseconds as graceMillis) is how long a fresh caller
// revocation is remembered while no entry needs it, so an in-flight Prepare
// still fails closed after a revoke.
const (
	defaultTTL         = 10 * time.Minute
	maxTTL             = 10 * time.Minute
	minServerKeyBytes  = 32
	defaultMaxActive   = 8
	limitMaxActive     = 100
	defaultPerCaller   = 4
	limitMaxPerCaller  = 100
	defaultTextBytes   = 8 << 20
	limitMaxTextBytes  = 64 << 20
	maxIdentityBytes   = 1024
	maxStatusBytes     = 64
	maxHandleBytes     = 128
	handleDigestDomain = "review-revision-prep-handle:v1:"
	maxSafeInteger     = 1<<53 - 1
	revocationGrace    = time.Minute
	graceMillis        = int64(revocationGrace / time.Millisecond)
)

// ErrServerKeyInvalid refuses a server key shorter than 32 bytes, the same
// minimum internal/importsource/inspection.go:82 enforces.
var ErrServerKeyInvalid = errors.New("REVIEW_REVISION_PREP_SECRET_INVALID")

// ErrOptionsInvalid refuses an unusable StoreOptions, matching
// IMPORT_RESOURCE_LIMIT_INVALID (internal/importsource/inspection.go:84). It is
// also the refusal for an id factory that returns an unusable id or one that
// would overwrite a live handle: both make the registry itself untrustworthy.
var ErrOptionsInvalid = errors.New("REVIEW_REVISION_PREP_LIMIT_INVALID")

// ErrBindingInvalid refuses a zero or partly empty Binding at every entry point,
// matching IMPORT_INSPECTION_INVALID (inspection.go:100). A binding must come
// from the authenticated session and project lease, never from the renderer.
var ErrBindingInvalid = errors.New("REVIEW_REVISION_PREP_BINDING_INVALID")

// ErrContextInvalid refuses a PreparedContext that cannot be the producer's
// output: empty or overlong identity fields, out-of-range integers, a text that
// is not valid UTF-8, a hash that is not 64 lowercase hex digits, or a hash that
// is not the sha256 of Text.
var ErrContextInvalid = errors.New("REVIEW_REVISION_PREP_CONTEXT_INVALID")

// ErrTextTooLarge refuses frozen text above maxTextBytes, either on its own or
// once the store's retained bytes are counted, mirroring
// IMPORT_SOURCE_BYTES_EXCEEDED (inspection.go:140).
var ErrTextTooLarge = errors.New("REVIEW_REVISION_PREP_CONTEXT_TOO_LARGE")

// ErrHandleUnknown refuses an id this store never minted, or one whose entry was
// already collected. The reference collapses this into
// IMPORT_INSPECTION_EXPIRED (import-inspection-store.ts:156, :168); the split is
// kept here because a preview/admission caller retries an unguessable handle very
// differently from a handle it never held, and knowing that a random 122-bit id
// exists tells an attacker nothing it did not already have to guess.
var ErrHandleUnknown = errors.New("REVIEW_REVISION_PREP_HANDLE_UNKNOWN")

// ErrHandleExpired refuses a handle past its expiry and drops the entry. The
// reference reports this through IMPORT_INSPECTION_EXPIRED; here it is distinct
// so the caller can tell "prepare again" from "you never had this handle".
var ErrHandleExpired = errors.New("REVIEW_REVISION_PREP_HANDLE_EXPIRED")

// ErrHandleRevoked refuses a handle minted by a prepare that raced a
// RevokeCaller, and any handle whose caller epoch moved. It is the Go port's
// revision check (inspection.go:137), which the TypeScript store does not have.
var ErrHandleRevoked = errors.New("REVIEW_REVISION_PREP_HANDLE_REVOKED")

// ErrCallerMismatch refuses a handle presented by a caller other than the one it
// was prepared for. The reference enforces this as IMPORT_INSPECTION_EXPIRED
// (import-inspection-store.ts:156, :165); a distinct code keeps a genuine bug
// visible instead of looking like an expiry.
var ErrCallerMismatch = errors.New("REVIEW_REVISION_PREP_CALLER_MISMATCH")

// ErrGenerationMismatch refuses a handle presented in a different generation
// than the one it was prepared for. This is the property the reference lacks:
// the successor of its purpose check (:165).
var ErrGenerationMismatch = errors.New("REVIEW_REVISION_PREP_GENERATION_MISMATCH")

// ErrPurposeMismatch refuses a handle presented under a different purpose, the
// exact analogue of the reference's purpose check
// (import-inspection-store.ts:163-168).
var ErrPurposeMismatch = errors.New("REVIEW_REVISION_PREP_PURPOSE_MISMATCH")

// ErrContextChanged is the compare-and-swap failure at Consume: the caller
// expected a different frozen hash than the one the handle holds. It mirrors
// IMPORT_INSPECTION_PREVIEW_CHANGED (inspection.go:167) and, like it, does not
// consume the handle.
var ErrContextChanged = errors.New("REVIEW_REVISION_PREP_CONTEXT_CHANGED")

// ErrLimit refuses a prepare that would exceed maxActive or maxPerCaller,
// mirroring IMPORT_INSPECTION_LIMIT (import-inspection-store.ts:122).
var ErrLimit = errors.New("REVIEW_REVISION_PREP_LIMIT")

// ErrStoreUnavailable refuses every call on a nil *Store instead of panicking in
// the mutex. The Go sibling does not guard this; a frozen-text registry that
// sits on the run-admission path is worth the two lines.
var ErrStoreUnavailable = errors.New("REVIEW_REVISION_PREP_STORE_UNAVAILABLE")

// Binding is the identity every Peek and Consume must match. All three fields
// are compared exactly, so surrounding whitespace is significant. The producer
// must fill them from the authenticated server session, the run generation and
// the review purpose, never from a renderer-supplied string: a caller that can
// choose its own generation could replay a handle into a run it does not own
// (internal/importsource/inspection.go:12-19).
type Binding struct {
	Caller     string
	Generation string
	Purpose    string
}

// PreparedContext is what a handle holds: the frozen review-revision context as
// the producer emitted it (Text and its sha256) plus the identity the admission
// needs to re-assert before it writes a ledger row. Every field is a value, so a
// copy is a deep copy: there is no slice, map or pointer through which a caller
// could reach back into the stored bytes. Context (the producer's ordered value)
// and AuthorInputs are deliberately absent: the consumer re-parses Text, which
// the producer guarantees is byte-for-byte plotJSON of that value
// (internal/store/review_revision_context.go:19-24), so this package needs no
// dependency on internal/store.
type PreparedContext struct {
	ProjectID     string
	Operation     string
	DraftID       int64
	ChapterNumber int64
	Version       int64
	Status        string
	SourceHash    string
	Text          string
	ContextHash   string
}

// StoreOptions configures a Store. Now is the clock seam and ID the id factory
// seam; both default to the reference behaviour when nil. TTL, MaxActive,
// MaxPerCaller and MaxTextBytes are applied only when zero, and are then
// validated, exactly as internal/importsource/inspection.go:83-84 does, so an
// explicit out-of-range value is refused rather than silently clamped.
type StoreOptions struct {
	Now          func() time.Time
	TTL          time.Duration
	MaxActive    int
	MaxPerCaller int
	MaxTextBytes int64
	ID           func() (string, error)
}

// Store is the in-process registry. It owns frozen text and a mutex, so it must
// never be copied: construct one with NewStore and share the *Store. The single
// mutex covers the map, the revision table and the accounting, and it is never
// held across a call that could block: the id factory runs outside it and the
// caller-supplied functions (Now, ID) are the only foreign calls, so Now is
// called only while the mutex is held but ID never is.
type Store struct {
	mu           sync.Mutex
	key          []byte
	now          func() time.Time
	ttl          time.Duration
	maxActive    int
	maxPerCaller int
	maxTextBytes int64
	id           func() (string, error)
	entries      map[string]record
	revisions    map[string]callerEpoch
}

// record is one live handle. expires is unix milliseconds so the fake clock in
// the tests and the sibling's removeExpiredLocked compare the same way
// (inspection.go:88). mac is a keyed digest over the record's own fields; it is
// not a wire receipt and never leaves the process, but it proves at Consume time
// that the frozen hash about to be released was minted by this store with this
// key rather than injected into the map.
type record struct {
	binding  Binding
	context  PreparedContext
	expires  int64
	revision uint64
	mac      string
}

// callerEpoch is the revision counter. revocation is the unix millisecond of the
// last RevokeCaller for this caller, or zero if it was never revoked; a zero
// epoch is indistinguishable from an absent one, which is what lets the prune
// drop it. The timestamp is why a prune keeps a fresh revocation for
// revocationGrace: an in-flight Prepare that sampled the old revision must still
// fail closed.
type callerEpoch struct {
	revision   uint64
	revocation int64
}

// NewStore builds a registry. serverKey keys the per-entry MAC; it must be 32
// bytes or longer (inspection.go:82) and it is copied, never retained by the
// caller's slice.
func NewStore(serverKey []byte, options StoreOptions) (*Store, error) {
	if len(serverKey) < minServerKeyBytes {
		return nil, ErrServerKeyInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ID == nil {
		options.ID = newHandleID
	}
	if options.TTL == 0 {
		options.TTL = defaultTTL
	}
	if options.MaxActive == 0 {
		options.MaxActive = defaultMaxActive
	}
	if options.MaxPerCaller == 0 {
		options.MaxPerCaller = defaultPerCaller
	}
	if options.MaxTextBytes == 0 {
		options.MaxTextBytes = defaultTextBytes
	}
	if options.TTL <= 0 || options.TTL > maxTTL ||
		options.MaxActive < 1 || options.MaxActive > limitMaxActive ||
		options.MaxPerCaller < 1 || options.MaxPerCaller > limitMaxPerCaller ||
		options.MaxTextBytes < 1 || options.MaxTextBytes > limitMaxTextBytes {
		return nil, ErrOptionsInvalid
	}
	return &Store{
		key:          append([]byte(nil), serverKey...),
		now:          options.Now,
		ttl:          options.TTL,
		maxActive:    options.MaxActive,
		maxPerCaller: options.MaxPerCaller,
		maxTextBytes: options.MaxTextBytes,
		id:           options.ID,
		entries:      make(map[string]record),
		revisions:    make(map[string]callerEpoch),
	}, nil
}

// Prepare binds an already frozen context to a new handle and returns its id.
// The id is minted outside the lock; the binding, the context and the caller's
// revision epoch are checked inside it, so a Prepare that races a RevokeCaller
// either publishes a handle the revoke will delete or fails closed with
// ErrHandleRevoked, never publishes one the revoke has already passed.
//
// Unlike import-inspection-store.ts:123-124 a prepare does not revoke the
// caller's earlier handles: review-revision may prepare several drafts before
// one of them is admitted, so replacement would silently kill a live selection.
// maxPerCaller, maxActive and the TTL are what bound that, and RevokeCaller is
// the explicit invalidation.
func (s *Store) Prepare(binding Binding, context PreparedContext) (string, error) {
	if s == nil {
		return "", ErrStoreUnavailable
	}
	if !validBinding(binding) {
		return "", ErrBindingInvalid
	}
	if err := validContext(context, s.maxTextBytes); err != nil {
		return "", err
	}
	// Sample the caller's epoch before minting. A revoke that lands between this
	// read and the insert moves the counter, and the insert then refuses.
	s.mu.Lock()
	revision := s.revisions[binding.Caller].revision
	s.mu.Unlock()

	id, err := s.id()
	if err != nil {
		return "", err
	}
	if !validLabel(id, maxHandleBytes) {
		return "", ErrOptionsInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked()
	if s.revisions[binding.Caller].revision != revision {
		return "", ErrHandleRevoked
	}
	if _, live := s.entries[id]; live {
		// A factory that repeats a live id would hand one caller another caller's
		// frozen bytes; refuse instead of overwriting.
		return "", ErrOptionsInvalid
	}
	if len(s.entries) >= s.maxActive {
		return "", ErrLimit
	}
	own := 0
	retained := int64(0)
	for _, item := range s.entries {
		if item.binding.Caller == binding.Caller {
			own++
		}
		retained += int64(len(item.context.Text))
	}
	if own >= s.maxPerCaller {
		return "", ErrLimit
	}
	if int64(len(context.Text)) > s.maxTextBytes-retained {
		return "", ErrTextTooLarge
	}
	item := record{
		binding:  binding,
		context:  context,
		expires:  s.now().Add(s.ttl).UnixMilli(),
		revision: revision,
	}
	item.mac = s.handleDigest(id, item)
	s.entries[id] = item
	return id, nil
}

// Peek resolves a handle without consuming it, for a caller that wants to show
// or re-check the frozen context before admitting it. It returns a value copy:
// mutating the result cannot change what a later Peek returns.
func (s *Store) Peek(id string, binding Binding) (PreparedContext, error) {
	if s == nil {
		return PreparedContext{}, ErrStoreUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.lookupLocked(id, binding)
	if err != nil {
		return PreparedContext{}, err
	}
	return item.context, nil
}

// Consume resolves a handle once, enforcing the same compare-and-swap the import
// store enforces (inspection.go:165-168): the caller must echo the frozen hash it
// expects, and a mismatch leaves the handle alive so a caller that guessed wrong
// can re-peek and retry. On success the handle is dead before the value is
// returned, so two consumers cannot both receive the same frozen bytes.
func (s *Store) Consume(id, expectedHash string, binding Binding) (PreparedContext, error) {
	if s == nil {
		return PreparedContext{}, ErrStoreUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, err := s.lookupLocked(id, binding)
	if err != nil {
		return PreparedContext{}, err
	}
	if !validDigest(expectedHash) || !hmac.Equal([]byte(item.context.ContextHash), []byte(expectedHash)) {
		return PreparedContext{}, ErrContextChanged
	}
	delete(s.entries, id)
	s.removeExpiredLocked()
	return item.context, nil
}

// RevokeCaller drops every handle the caller holds, whatever its generation or
// purpose, and moves the caller's revision counter so an in-flight Prepare for
// that caller cannot republish a handle the revoke has passed. This is the
// page-reload and reselection path, and the reason a long prepare cannot leak an
// orphan: nothing depends on the caller remembering to clean up.
func (s *Store) RevokeCaller(caller string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropExpiredLocked()
	epoch := s.revisions[caller]
	epoch.revision++
	epoch.revocation = s.now().UnixMilli()
	s.revisions[caller] = epoch
	for id, item := range s.entries {
		if item.binding.Caller == caller {
			delete(s.entries, id)
		}
	}
	s.pruneEpochsLocked(s.now().UnixMilli())
}

// ActiveCount reports how many handles are live right now and evicts the
// expired ones on the way, exactly as import-inspection-store.ts:178-181 does, so
// an operator scraping this counter cannot keep memory alive.
func (s *Store) ActiveCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpiredLocked()
	return len(s.entries)
}

// lookupLocked is the one admission point for a handle. Order matters: binding
// equality must be decided before the revision comparison, otherwise an id
// presented by a caller with an untouched epoch (revision zero) would be
// reported as revoked instead of as a caller mismatch.
func (s *Store) lookupLocked(id string, binding Binding) (record, error) {
	if !validBinding(binding) {
		return record{}, ErrBindingInvalid
	}
	if id == "" || len(id) > maxHandleBytes || !utf8.ValidString(id) {
		return record{}, ErrHandleUnknown
	}
	item, live := s.entries[id]
	if !live {
		return record{}, ErrHandleUnknown
	}
	if s.now().UnixMilli() >= item.expires {
		delete(s.entries, id)
		return record{}, ErrHandleExpired
	}
	if !hmac.Equal([]byte(s.handleDigest(id, item)), []byte(item.mac)) {
		// A record that cannot be proved to come from this store's Prepare is not
		// trusted at all, and it is dropped rather than kept.
		delete(s.entries, id)
		return record{}, ErrHandleUnknown
	}
	if item.binding.Caller != binding.Caller {
		return record{}, ErrCallerMismatch
	}
	if item.binding.Generation != binding.Generation {
		return record{}, ErrGenerationMismatch
	}
	if item.binding.Purpose != binding.Purpose {
		return record{}, ErrPurposeMismatch
	}
	if s.revisions[binding.Caller].revision != item.revision {
		delete(s.entries, id)
		return record{}, ErrHandleRevoked
	}
	return item, nil
}

// removeExpiredLocked is the memory cap: drop every expired entry, then forget
// every caller epoch that no live entry still needs. A fresh revocation is kept
// for revocationGrace, because it is the only thing that makes an in-flight
// Prepare fail closed; an old one is dropped so a process that serves many
// sessions cannot accumulate a caller per session forever.
func (s *Store) removeExpiredLocked() {
	s.dropExpiredLocked()
	s.pruneEpochsLocked(s.now().UnixMilli())
}

// dropExpiredLocked evicts expired entries only.
func (s *Store) dropExpiredLocked() {
	now := s.now().UnixMilli()
	for id, item := range s.entries {
		if item.expires <= now {
			delete(s.entries, id)
		}
	}
}

// pruneEpochsLocked forgets epochs no live entry needs and no recent revocation
// protects.
func (s *Store) pruneEpochsLocked(now int64) {
	if len(s.revisions) == 0 {
		return
	}
	active := make(map[string]bool, len(s.entries))
	for _, item := range s.entries {
		active[item.binding.Caller] = true
	}
	for caller, epoch := range s.revisions {
		if active[caller] {
			continue
		}
		if epoch.revocation == 0 || now-epoch.revocation > graceMillis {
			delete(s.revisions, caller)
		}
	}
}

// handleDigest binds a record to this store's key. Every field is NUL-free or
// decimal, so the separator is unambiguous.
func (s *Store) handleDigest(id string, item record) string {
	value := id + "\x00" + item.binding.Caller + "\x00" + item.binding.Generation + "\x00" +
		item.binding.Purpose + "\x00" + strconv.FormatInt(item.expires, 10) + "\x00" +
		item.context.ContextHash + "\x00" + strconv.Itoa(len(item.context.Text)) + "\x00" +
		strconv.FormatUint(item.revision, 10)
	return keyedDigest(s.key, handleDigestDomain, value)
}

// validBinding refuses a zero or partly empty Binding. Lengths and NUL are
// checked as well as emptiness (inspection.go:87), and the comparison itself is
// exact, so a padded caller never matches a stored one.
func validBinding(binding Binding) bool {
	return validLabel(binding.Caller, maxIdentityBytes) &&
		validLabel(binding.Generation, maxIdentityBytes) &&
		validLabel(binding.Purpose, maxIdentityBytes)
}

// validContext refuses a PreparedContext that the freeze producer could not have
// produced. The hash is re-derived from Text, because the hash is the contract
// (internal/store/review_revision_context.go:26) and the admission recomputes it
// at internal/admission/reviewcycle_effect.go:189-190; a mismatched pair would
// make Consume's compare-and-swap compare the wrong bytes.
func validContext(context PreparedContext, maxTextBytes int64) error {
	if !validLabel(context.ProjectID, maxIdentityBytes) ||
		!validLabel(context.Operation, maxIdentityBytes) ||
		!validLabel(context.Status, maxStatusBytes) {
		return ErrContextInvalid
	}
	if context.DraftID < 0 || context.DraftID > maxSafeInteger ||
		context.ChapterNumber < 0 || context.ChapterNumber > maxSafeInteger ||
		context.Version < 0 || context.Version > maxSafeInteger {
		return ErrContextInvalid
	}
	if context.SourceHash != "" && !validDigest(context.SourceHash) {
		return ErrContextInvalid
	}
	if context.Text == "" || !utf8.ValidString(context.Text) ||
		strings.ContainsRune(context.Text, 0) || !validDigest(context.ContextHash) {
		return ErrContextInvalid
	}
	if int64(len(context.Text)) > maxTextBytes {
		return ErrTextTooLarge
	}
	if !hmac.Equal([]byte(textHash(context.Text)), []byte(context.ContextHash)) {
		return ErrContextInvalid
	}
	return nil
}

// validLabel reports whether value can serve as an exact-match identity string.
func validLabel(value string, max int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= max &&
		utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// validDigest accepts 64 lowercase hex digits (inspection.go, validDigest).
func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// newHandleID is the default id factory. It is byte-identical to the reference's
// derivation, electron/services/import-inspection-store.ts:125
// (`const inspectionId = randomUUID()`): a lowercase RFC 4122 version 4 UUID in
// 8-4-4-4-12 form over 122 bits of crypto/rand, and the same function as
// internal/importsource/identity.go:68-76. The fallback shape this task allows
// when a reference derives no id (256 random bits in unpadded base64url) is
// therefore not used: the reference does derive one, and matching it keeps the
// handle format identical to the sibling's.
func newHandleID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// textHash is the producer's context hash: sha256 hex over the frozen bytes
// (internal/store/summary.go:35-38).
func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// keyedDigest is the port's keyed digest (internal/importsource/identity.go:52-57).
func keyedDigest(key []byte, domain, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
