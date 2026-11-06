package librarypath

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Bounds. defaultTTL and maxGrantTTL are the desktop's own external-file grant
// window (electron/controllers/external-file-grant-controller.ts:18 pins ten
// minutes for an export directory), minSecretBytes is the same 32-byte key floor
// internal/importsource/inspection.go:82 enforces, and the max* limits bound how
// much a single tab, or the process, can leave behind. grantDigestDomain
// separates the entry MAC from any other use of the same key, and
// grantRevocationGrace is how long a caller epoch is remembered while no entry
// needs it, so an in-flight Mint still fails closed after a revoke.
const (
	defaultGrantTTL      = 10 * time.Minute
	maxGrantTTL          = 10 * time.Minute
	minSecretBytes       = 32
	defaultMaxActive     = 256
	limitMaxActive       = 4096
	defaultMaxPerCaller  = 64
	limitMaxPerCaller    = 1024
	defaultMaxUses       = 1
	limitMaxUses         = 1 << 16
	maxTokenBytes        = 128
	maxIdentityBytes     = 256
	grantDigestDomain    = "library-path-grant:v1:"
	grantRevocationGrace = time.Minute
	grantGraceMillis     = int64(grantRevocationGrace / time.Millisecond)
)

// The registry's refusals. Each is symbolic, and each answers a different
// question: "you never had this token", "it expired", "it was revoked", "another
// caller holds it", "it may not do that". The renderer only needs the code, but a
// test - and an operator reading a log - needs the distinction.
var (
	// ErrSecretInvalid refuses a server key shorter than 32 bytes.
	ErrSecretInvalid = errors.New("LIBRARY_PATH_GRANT_SECRET_INVALID")
	// ErrOptionsInvalid refuses unusable registry options, and a token factory
	// that produced an unusable or already live token.
	ErrOptionsInvalid = errors.New("LIBRARY_PATH_GRANT_LIMIT_INVALID")
	// ErrBindingInvalid refuses a zero or partly empty Binding. A binding must
	// come from the authenticated session and tab, never from a request body.
	ErrBindingInvalid = errors.New("LIBRARY_PATH_GRANT_BINDING_INVALID")
	// ErrTargetInvalid refuses a Target the allowlist could not have produced, or
	// an empty operation set.
	ErrTargetInvalid = errors.New("LIBRARY_PATH_GRANT_TARGET_INVALID")
	// ErrGrantUnknown refuses a token this registry never minted, or one whose
	// record could not be proved to come from it.
	ErrGrantUnknown = errors.New("LIBRARY_PATH_GRANT_UNKNOWN")
	// ErrGrantExpired refuses a token past its expiry and drops the record.
	ErrGrantExpired = errors.New("LIBRARY_PATH_GRANT_EXPIRED")
	// ErrGrantRevoked refuses a token that was revoked, and any token whose
	// caller epoch moved after it was minted.
	ErrGrantRevoked = errors.New("LIBRARY_PATH_GRANT_REVOKED")
	// ErrGrantExhausted refuses a token whose usage budget is spent. The record
	// is kept so a later caller learns the difference from "never existed".
	ErrGrantExhausted = errors.New("LIBRARY_PATH_GRANT_EXHAUSTED")
	// ErrGrantOperationDenied refuses an operation the token does not carry.
	ErrGrantOperationDenied = errors.New("LIBRARY_PATH_GRANT_OPERATION_DENIED")
	// ErrCallerMismatch refuses a token presented by a different cookie session
	// or a different tab than the one it was minted for.
	ErrCallerMismatch = errors.New("LIBRARY_PATH_GRANT_CALLER_MISMATCH")
	// ErrGrantLimit refuses a mint that would exceed the active or per-caller cap.
	ErrGrantLimit = errors.New("LIBRARY_PATH_GRANT_LIMIT")
	// ErrRegistryUnavailable refuses every call on a nil *Registry instead of
	// panicking inside the mutex.
	ErrRegistryUnavailable = errors.New("LIBRARY_PATH_GRANT_REGISTRY_UNAVAILABLE")
)

// Op is one operation a grant may carry. The set is deliberately small: these are
// the only three things a browser grant is allowed to cause, and none of them is a
// write into author data (that is refused by Target.requireOutput).
type Op uint8

const (
	// OpRead may read one file through the grant's target.
	OpRead Op = 1 << 0
	// OpWrite may replace one existing file, or create one when OpMkdir is also
	// carried, inside the grant's target.
	OpWrite Op = 1 << 1
	// OpMkdir may create directories inside the grant's target, which is also what
	// lets OpWrite create a new file (the desktop's separate "create" permission,
	// electron/controllers/external-file-grant-controller.ts:139-152).
	OpMkdir Op = 1 << 2
)

const opAll = OpRead | OpWrite | OpMkdir

func (o Op) valid() bool { return o != 0 && o&^opAll == 0 }

// Permits reports whether a grant carrying o may also do required.
func (o Op) Permits(required Op) bool { return required != 0 && o&required == required }

func (o Op) String() string {
	if !o.valid() {
		return ""
	}
	names := make([]string, 0, 3)
	for _, item := range []struct {
		op   Op
		name string
	}{{OpRead, "read"}, {OpWrite, "write"}, {OpMkdir, "mkdir"}} {
		if o&item.op != 0 {
			names = append(names, item.name)
		}
	}
	return strings.Join(names, ",")
}

// Binding is the caller a token belongs to: the cookie session and the browser
// tab, spelled exactly as projectsession.Owner spells them
// (internal/projectsession/session.go:28-30). Both fields are compared exactly.
type Binding struct {
	SessionID string
	ClientID  string
}

// Valid reports whether a binding could have come from the authenticated
// session. A ':' or a NUL would make two different callers share one epoch key.
func (b Binding) Valid() bool {
	return validLabel(b.SessionID) && validLabel(b.ClientID)
}

// key is the epoch key. It is unambiguous because Valid refuses the separator.
func (b Binding) key() string { return b.SessionID + "\x00" + b.ClientID }

// MintOptions configures one token. Zero means "the registry default", so a
// single-use token with the registry TTL needs no options at all.
type MintOptions struct {
	TTL     time.Duration
	MaxUses int64
}

// RegistryOptions configures a Registry. Now and Token are the clock and token
// seams; both default to the production behaviour. TTL, MaxActive and MaxPerCaller
// are applied only when zero and are then validated, so an explicit out-of-range
// value is refused rather than silently clamped.
type RegistryOptions struct {
	Now          func() time.Time
	Token        func() (string, error)
	TTL          time.Duration
	MaxActive    int
	MaxPerCaller int
}

// Grant is what a redeem answers: the resolved path plus the operations and the
// budget that remain. It is a value, so a caller cannot reach back into the store.
type Grant struct {
	Target    Target
	Ops       Op
	Expires   int64
	Remaining int64
}

// Registry is the in-process token store. It owns a mutex, so construct one and
// share the *Registry. The mutex covers the map, the epoch table and the usage
// accounting and is never held across a foreign call: the token factory runs
// outside it, and Now is the only injected function called while it is held
// (exactly the split internal/reviewrevprep/store.go:326-352 uses).
type Registry struct {
	mu           sync.Mutex
	key          []byte
	now          func() time.Time
	token        func() (string, error)
	ttl          time.Duration
	maxActive    int
	maxPerCaller int
	entries      map[string]record
	revisions    map[string]callerEpoch
}

// record is one minted token. expires is unix milliseconds so the fake clock in
// the tests and the production clock compare the same way. uses is the usage
// record and remaining its budget; both are only ever changed under the mutex,
// together with a fresh MAC, so a read-modify-write can never leave the record
// half updated. revoked keeps a tombstone until expiry so a revoked token answers
// "revoked" rather than "unknown". mac is a keyed digest over the record's own
// fields; it never leaves the process, and it proves at redeem time that the
// record was minted by this registry with this key rather than injected into the
// map.
type record struct {
	binding   Binding
	target    Target
	ops       Op
	expires   int64
	remaining int64
	uses      int64
	revoked   int64
	revision  uint64
	mac       string
}

// callerEpoch is the revision counter, the same shape as reviewrevprep's: a
// revoke moves it, so a Mint that sampled the old value fails closed instead of
// publishing a token the revoke has already passed.
type callerEpoch struct {
	revision   uint64
	revocation int64
}

// NewRegistry builds a token store. serverKey keys each record's MAC; it must be
// at least 32 bytes and it is copied, never retained by the caller's slice.
func NewRegistry(serverKey []byte, options RegistryOptions) (*Registry, error) {
	if len(serverKey) < minSecretBytes {
		return nil, ErrSecretInvalid
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Token == nil {
		options.Token = tokenID
	}
	if options.TTL == 0 {
		options.TTL = defaultGrantTTL
	}
	if options.MaxActive == 0 {
		options.MaxActive = defaultMaxActive
	}
	if options.MaxPerCaller == 0 {
		options.MaxPerCaller = defaultMaxPerCaller
	}
	if options.TTL <= 0 || options.TTL > maxGrantTTL ||
		options.MaxActive < 1 || options.MaxActive > limitMaxActive ||
		options.MaxPerCaller < 1 || options.MaxPerCaller > limitMaxPerCaller {
		return nil, ErrOptionsInvalid
	}
	return &Registry{
		key:          append([]byte(nil), serverKey...),
		now:          options.Now,
		token:        options.Token,
		ttl:          options.TTL,
		maxActive:    options.MaxActive,
		maxPerCaller: options.MaxPerCaller,
		entries:      make(map[string]record),
		revisions:    make(map[string]callerEpoch),
	}, nil
}

// Mint binds one already resolved target to a new token for one caller. The token
// is minted outside the lock; the caller's epoch is sampled before it, so a mint
// that races RevokeCaller either publishes a token the revoke will delete or fails
// with ErrGrantRevoked - it can never publish one the revoke has already passed.
func (r *Registry) Mint(binding Binding, target Target, ops Op, options MintOptions) (string, error) {
	if r == nil {
		return "", ErrRegistryUnavailable
	}
	if !binding.Valid() {
		return "", ErrBindingInvalid
	}
	if !target.Valid() || !ops.valid() {
		return "", ErrTargetInvalid
	}
	if options.TTL == 0 {
		options.TTL = r.ttl
	}
	if options.MaxUses == 0 {
		options.MaxUses = defaultMaxUses
	}
	if options.TTL < 0 || options.TTL > maxGrantTTL || options.MaxUses < 0 || options.MaxUses > limitMaxUses {
		return "", ErrOptionsInvalid
	}
	epochKey := binding.key()
	r.mu.Lock()
	revision := r.revisions[epochKey].revision
	r.mu.Unlock()

	token, err := r.token()
	if err != nil {
		return "", err
	}
	if !validTokenID(token) {
		return "", ErrOptionsInvalid
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	if r.revisions[epochKey].revision != revision {
		return "", ErrGrantRevoked
	}
	if _, live := r.entries[token]; live {
		// A factory that repeats a live token would hand one caller another
		// caller's path authority; refuse instead of overwriting.
		return "", ErrOptionsInvalid
	}
	active, own := 0, 0
	for _, item := range r.entries {
		if item.revoked != 0 {
			continue
		}
		active++
		if item.binding == binding {
			own++
		}
	}
	if active >= r.maxActive || own >= r.maxPerCaller {
		return "", ErrGrantLimit
	}
	item := record{
		binding:   binding,
		target:    target,
		ops:       ops,
		expires:   r.now().Add(options.TTL).UnixMilli(),
		remaining: options.MaxUses,
		revision:  revision,
	}
	item.mac = r.recordDigest(token, item)
	r.entries[token] = item
	return token, nil
}

// Redeem spends one use of a token and returns the target it authorises. The
// order of the checks is the admission policy: expiry, then the MAC (a record
// that cannot be proved to come from this registry is dropped, not trusted), then
// the caller, then the epoch, then the operation, then the budget. The caller
// comparison precedes the epoch comparison deliberately - an epoch nobody moved
// is zero, so the other order would report a foreign token as revoked. The
// decrement and the re-MAC happen under the same lock hold as the read, which is
// what makes the usage record single-writer safe: two concurrent callers cannot
// both spend the last use.
func (r *Registry) Redeem(token string, binding Binding, required Op) (Grant, error) {
	if r == nil {
		return Grant{}, ErrRegistryUnavailable
	}
	if !binding.Valid() {
		return Grant{}, ErrBindingInvalid
	}
	if !required.valid() {
		return Grant{}, ErrGrantOperationDenied
	}
	if !validTokenID(token) {
		return Grant{}, ErrGrantUnknown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, live := r.entries[token]
	if !live {
		return Grant{}, ErrGrantUnknown
	}
	if r.now().UnixMilli() >= item.expires {
		delete(r.entries, token)
		return Grant{}, ErrGrantExpired
	}
	if !hmac.Equal([]byte(r.recordDigest(token, item)), []byte(item.mac)) {
		delete(r.entries, token)
		return Grant{}, ErrGrantUnknown
	}
	if item.binding != binding {
		return Grant{}, ErrCallerMismatch
	}
	if r.revisions[binding.key()].revision != item.revision {
		delete(r.entries, token)
		return Grant{}, ErrGrantRevoked
	}
	if item.revoked != 0 {
		return Grant{}, ErrGrantRevoked
	}
	if !item.ops.Permits(required) {
		return Grant{}, ErrGrantOperationDenied
	}
	if item.remaining <= 0 {
		return Grant{}, ErrGrantExhausted
	}
	item.remaining--
	item.uses++
	item.mac = r.recordDigest(token, item)
	r.entries[token] = item
	return Grant{Target: item.target, Ops: item.ops, Expires: item.expires, Remaining: item.remaining}, nil
}

// Revoke drops one token and leaves a tombstone until its expiry, so a page that
// keeps replaying it learns "revoked" instead of "never existed". It reports
// whether the token was still usable.
func (r *Registry) Revoke(token string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	item, live := r.entries[token]
	if !live || item.revoked != 0 {
		return false
	}
	item.revoked = r.now().UnixMilli()
	item.mac = r.recordDigest(token, item)
	r.entries[token] = item
	return true
}

// RevokeCaller drops every token a caller holds and moves its epoch, so an
// in-flight Mint for that caller cannot republish one the revoke has passed. This
// is the page-reload, tab-close and reselection path: nothing depends on the page
// remembering to clean up. It returns how many live tokens it dropped.
func (r *Registry) RevokeCaller(binding Binding) int {
	if r == nil || !binding.Valid() {
		return 0
	}
	epochKey := binding.key()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UnixMilli()
	epoch := r.revisions[epochKey]
	epoch.revision++
	epoch.revocation = now
	r.revisions[epochKey] = epoch
	dropped := 0
	for token, item := range r.entries {
		if item.binding == binding {
			if item.revoked == 0 {
				dropped++
			}
			delete(r.entries, token)
		}
	}
	r.pruneEpochsLocked(now)
	return dropped
}

// ActiveCount reports how many tokens are usable right now and evicts the expired
// ones on the way, so an operator scraping this counter cannot keep memory alive.
func (r *Registry) ActiveCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	active := 0
	for _, item := range r.entries {
		if item.revoked == 0 {
			active++
		}
	}
	return active
}

// pruneLocked drops expired records and then every caller epoch no live record
// still needs and no recent revocation protects.
func (r *Registry) pruneLocked() {
	now := r.now().UnixMilli()
	for token, item := range r.entries {
		if item.expires <= now {
			delete(r.entries, token)
		}
	}
	r.pruneEpochsLocked(now)
}

// pruneEpochsLocked forgets epochs no live record needs. A fresh revocation is
// kept for grantRevocationGrace because it is the only thing that makes an
// in-flight Mint fail closed; an old one is dropped so a long-running process
// cannot accumulate one entry per session forever.
func (r *Registry) pruneEpochsLocked(now int64) {
	if len(r.revisions) == 0 {
		return
	}
	active := make(map[string]bool, len(r.entries))
	for _, item := range r.entries {
		active[item.binding.key()] = true
	}
	for epochKey, epoch := range r.revisions {
		if active[epochKey] {
			continue
		}
		if epoch.revocation == 0 || now-epoch.revocation > grantGraceMillis {
			delete(r.revisions, epochKey)
		}
	}
}

// recordDigest binds a record to this registry's key. Every field is NUL-free or
// decimal, so the separator is unambiguous; the MAC is recomputed on every
// mutation because the usage record is part of what is being proved.
func (r *Registry) recordDigest(token string, item record) string {
	value := strings.Join([]string{
		token,
		item.binding.SessionID,
		item.binding.ClientID,
		item.target.RootID,
		item.target.Kind.String(),
		item.target.Root,
		item.target.Relative,
		item.target.Path,
		strconv.FormatUint(uint64(item.ops), 10),
		strconv.FormatInt(item.expires, 10),
		strconv.FormatInt(item.remaining, 10),
		strconv.FormatInt(item.uses, 10),
		strconv.FormatInt(item.revoked, 10),
		strconv.FormatUint(item.revision, 10),
	}, "\x00")
	return keyedDigest(r.key, grantDigestDomain, value)
}

// validLabel reports whether value can serve as an exact-match identity string.
func validLabel(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maxIdentityBytes &&
		utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// validTokenID bounds a token. The token is the map key, so it must be present,
// bounded and free of NUL.
func validTokenID(token string) bool {
	return token != "" && len(token) <= maxTokenBytes && utf8.ValidString(token) &&
		!strings.ContainsRune(token, 0)
}

// tokenID is the default token factory: 32 bytes of crypto/rand in unpadded
// base64url, so 256 bits of entropy - at least as hard to guess as the review
// context handles this package's Registry is modelled on, which are 122-bit
// version 4 UUIDs (internal/reviewrevprep/store.go:624-632).
func tokenID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// keyedDigest is the keyed digest this repository uses for capability records
// (internal/importsource/identity.go:52-57).
func keyedDigest(key []byte, domain, value string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}
