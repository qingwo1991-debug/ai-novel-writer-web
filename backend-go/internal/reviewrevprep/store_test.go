package reviewrevprep

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey is a 32-byte server key, the minimum NewStore accepts.
const testKey = "0123456789abcdef0123456789abcdef"

// refusalCodes pins every refusal to its exact wire code in one place, so a
// silent rename of a sentinel fails the whole suite instead of passing
// errors.Is.
var refusalCodes = map[error]string{
	ErrServerKeyInvalid:   "REVIEW_REVISION_PREP_SECRET_INVALID",
	ErrOptionsInvalid:     "REVIEW_REVISION_PREP_LIMIT_INVALID",
	ErrBindingInvalid:     "REVIEW_REVISION_PREP_BINDING_INVALID",
	ErrContextInvalid:     "REVIEW_REVISION_PREP_CONTEXT_INVALID",
	ErrTextTooLarge:       "REVIEW_REVISION_PREP_CONTEXT_TOO_LARGE",
	ErrHandleUnknown:      "REVIEW_REVISION_PREP_HANDLE_UNKNOWN",
	ErrHandleExpired:      "REVIEW_REVISION_PREP_HANDLE_EXPIRED",
	ErrHandleRevoked:      "REVIEW_REVISION_PREP_HANDLE_REVOKED",
	ErrCallerMismatch:     "REVIEW_REVISION_PREP_CALLER_MISMATCH",
	ErrGenerationMismatch: "REVIEW_REVISION_PREP_GENERATION_MISMATCH",
	ErrPurposeMismatch:    "REVIEW_REVISION_PREP_PURPOSE_MISMATCH",
	ErrContextChanged:     "REVIEW_REVISION_PREP_CONTEXT_CHANGED",
	ErrLimit:              "REVIEW_REVISION_PREP_LIMIT",
	ErrStoreUnavailable:   "REVIEW_REVISION_PREP_STORE_UNAVAILABLE",
}

// fakeClock is the clock seam: no test sleeps and no test depends on wall time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

// testHash recomputes the producer's hash independently of the package helper.
func testHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func testContextText() string {
	return `{"version":1,"operation":"review-chapter","source":{"id":7,"chapterNumber":3}}`
}

func testPreparedContext() PreparedContext {
	text := testContextText()
	return PreparedContext{
		ProjectID:     "project-alpha",
		Operation:     "review-chapter",
		DraftID:       7,
		ChapterNumber: 3,
		Version:       2,
		Status:        "revised",
		SourceHash:    testHash("draft body"),
		Text:          text,
		ContextHash:   testHash(text),
	}
}

func testBinding(caller string) Binding {
	return Binding{Caller: caller, Generation: "run-1", Purpose: "review-revision"}
}

func newTestStore(t *testing.T, clock *fakeClock, options StoreOptions) *Store {
	t.Helper()
	options.Now = clock.Now
	store, err := NewStore([]byte(testKey), options)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store == nil {
		t.Fatal("NewStore returned nil without an error")
	}
	return store
}

func mustPrepare(t *testing.T, store *Store, binding Binding, context PreparedContext) string {
	t.Helper()
	id, err := store.Prepare(binding, context)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if id == "" {
		t.Fatal("Prepare returned an empty handle")
	}
	return id
}

// requireCode asserts both the sentinel and the exact code string.
func requireCode(t *testing.T, err error, want error) {
	t.Helper()
	code, known := refusalCodes[want]
	if !known {
		t.Fatalf("no expected code recorded for %v", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if err.Error() != code {
		t.Fatalf("error code = %v, want %v", err.Error(), code)
	}
}

func TestPeekIsRepeatable(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	binding := testBinding("alice")
	context := testPreparedContext()
	id := mustPrepare(t, store, binding, context)
	for attempt := 0; attempt < 3; attempt++ {
		got, err := store.Peek(id, binding)
		if err != nil {
			t.Fatalf("Peek %d: %v", attempt, err)
		}
		if got != context {
			t.Fatalf("Peek %d returned a different context", attempt)
		}
	}
	if count := store.ActiveCount(); count != 1 {
		t.Fatalf("ActiveCount = %d, want 1", count)
	}
}

func TestMismatchedBindingIsRefused(t *testing.T) {
	cases := []struct {
		name       string
		caller     string
		generation string
		purpose    string
		want       error
	}{
		{"other caller", "mallory", "run-1", "review-revision", ErrCallerMismatch},
		{"other generation", "alice", "run-2", "review-revision", ErrGenerationMismatch},
		{"other purpose", "alice", "run-1", "review-progress", ErrPurposeMismatch},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock, StoreOptions{})
			id := mustPrepare(t, store, testBinding("alice"), testPreparedContext())
			probe := Binding{Caller: testCase.caller, Generation: testCase.generation, Purpose: testCase.purpose}
			if _, err := store.Peek(id, probe); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatalf("Peek with %v succeeded, want %v", testCase.name, testCase.want)
			}
			if _, err := store.Consume(id, testPreparedContext().ContextHash, probe); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatalf("Consume with %v succeeded, want %v", testCase.name, testCase.want)
			}
			// A refused probe must not consume: the owner still holds the handle.
			if count := store.ActiveCount(); count != 1 {
				t.Fatalf("ActiveCount = %d, want 1", count)
			}
			if _, err := store.Peek(id, testBinding("alice")); err != nil {
				t.Fatalf("owner Peek after refused probe: %v", err)
			}
		})
	}
}

func TestExpiredHandleIsRefusedAndEvicted(t *testing.T) {
	cases := []struct {
		name string
		act  func(t *testing.T, store *Store, id string, binding Binding)
	}{
		{"peek", func(t *testing.T, store *Store, id string, binding Binding) {
			if _, err := store.Peek(id, binding); err != nil {
				requireCode(t, err, ErrHandleExpired)
			} else {
				t.Fatal("Peek of an expired handle succeeded")
			}
		}},
		{"consume", func(t *testing.T, store *Store, id string, binding Binding) {
			if _, err := store.Consume(id, testPreparedContext().ContextHash, binding); err != nil {
				requireCode(t, err, ErrHandleExpired)
			} else {
				t.Fatal("Consume of an expired handle succeeded")
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock, StoreOptions{TTL: 2 * time.Minute})
			binding := testBinding("alice")
			id := mustPrepare(t, store, binding, testPreparedContext())
			clock.Advance(119 * time.Second)
			if _, err := store.Peek(id, binding); err != nil {
				t.Fatalf("Peek before expiry: %v", err)
			}
			clock.Advance(time.Second)
			testCase.act(t, store, id, binding)
			if count := store.ActiveCount(); count != 0 {
				t.Fatalf("ActiveCount after expiry = %d, want 0", count)
			}
			// The entry is gone, not merely hidden.
			if _, err := store.Peek(id, binding); err != nil {
				requireCode(t, err, ErrHandleUnknown)
			} else {
				t.Fatal("Peek of an evicted handle succeeded")
			}
		})
	}
}

func TestConsumeEnforcesContextHash(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	binding := testBinding("alice")
	context := testPreparedContext()
	id := mustPrepare(t, store, binding, context)

	wrongs := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"truncated", context.ContextHash[:63]},
		{"uppercase", strings.ToUpper(context.ContextHash)},
		{"other digest", testHash("another frozen context")},
	}
	for _, wrong := range wrongs {
		t.Run("wrong hash "+wrong.name, func(t *testing.T) {
			if _, err := store.Consume(id, wrong.hash, binding); err != nil {
				requireCode(t, err, ErrContextChanged)
			} else {
				t.Fatal("Consume with a wrong hash succeeded")
			}
			// A failed compare-and-swap leaves the handle alive.
			if count := store.ActiveCount(); count != 1 {
				t.Fatalf("ActiveCount after failed consume = %d, want 1", count)
			}
			if _, err := store.Peek(id, binding); err != nil {
				t.Fatalf("Peek after failed consume: %v", err)
			}
		})
	}

	got, err := store.Consume(id, context.ContextHash, binding)
	if err != nil {
		t.Fatalf("Consume with the right hash: %v", err)
	}
	if got != context {
		t.Fatal("Consume returned a different context")
	}
	if count := store.ActiveCount(); count != 0 {
		t.Fatalf("ActiveCount after consume = %d, want 0", count)
	}
	if _, err := store.Peek(id, binding); err != nil {
		requireCode(t, err, ErrHandleUnknown)
	} else {
		t.Fatal("Peek after consume succeeded")
	}
	if _, err := store.Consume(id, context.ContextHash, binding); err != nil {
		requireCode(t, err, ErrHandleUnknown)
	} else {
		t.Fatal("second Consume succeeded")
	}
}

func TestRevokeCallerInvalidatesOnlyThatCaller(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	alice := testBinding("alice")
	bob := testBinding("bob")
	aliceFirst := mustPrepare(t, store, alice, testPreparedContext())
	aliceSecond := mustPrepare(t, store, alice, testPreparedContext())
	bobID := mustPrepare(t, store, bob, testPreparedContext())
	if count := store.ActiveCount(); count != 3 {
		t.Fatalf("ActiveCount = %d, want 3", count)
	}

	store.RevokeCaller("alice")

	if count := store.ActiveCount(); count != 1 {
		t.Fatalf("ActiveCount after revoke = %d, want 1", count)
	}
	for _, id := range []string{aliceFirst, aliceSecond} {
		if _, err := store.Peek(id, alice); err != nil {
			requireCode(t, err, ErrHandleUnknown)
		} else {
			t.Fatal("revoked handle is still live")
		}
	}
	if _, err := store.Peek(bobID, bob); err != nil {
		t.Fatalf("bob Peek after alice revoke: %v", err)
	}
	// A caller that was revoked may prepare again and be consumed normally.
	again := mustPrepare(t, store, alice, testPreparedContext())
	if _, err := store.Consume(again, testPreparedContext().ContextHash, alice); err != nil {
		t.Fatalf("Consume after re-prepare: %v", err)
	}
	// Revoking a caller that holds nothing is not an error.
	store.RevokeCaller("nobody")
	if _, err := store.Peek(bobID, bob); err != nil {
		t.Fatalf("bob Peek after unrelated revoke: %v", err)
	}
}

func TestPrepareRefusesEmptyBinding(t *testing.T) {
	cases := []struct {
		name    string
		binding Binding
		want    error
	}{
		{"zero binding", Binding{}, ErrBindingInvalid},
		{"no caller", Binding{Generation: "run-1", Purpose: "review-revision"}, ErrBindingInvalid},
		{"no generation", Binding{Caller: "alice", Purpose: "review-revision"}, ErrBindingInvalid},
		{"no purpose", Binding{Caller: "alice", Generation: "run-1"}, ErrBindingInvalid},
		{"blank caller", Binding{Caller: "   ", Generation: "run-1", Purpose: "review-revision"}, ErrBindingInvalid},
		{"nul caller", Binding{Caller: "al\x00ice", Generation: "run-1", Purpose: "review-revision"}, ErrBindingInvalid},
		{"overlong generation", Binding{Caller: "alice", Generation: strings.Repeat("g", 1025), Purpose: "review-revision"}, ErrBindingInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock, StoreOptions{})
			if _, err := store.Prepare(testCase.binding, testPreparedContext()); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatal("Prepare accepted an empty binding")
			}
			if _, err := store.Peek("handle", testCase.binding); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatal("Peek accepted an empty binding")
			}
			if _, err := store.Consume("handle", testPreparedContext().ContextHash, testCase.binding); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatal("Consume accepted an empty binding")
			}
			if count := store.ActiveCount(); count != 0 {
				t.Fatalf("ActiveCount = %d, want 0", count)
			}
		})
	}
}

func TestPrepareRefusesInvalidContext(t *testing.T) {
	cases := []struct {
		name         string
		adjust       func(context *PreparedContext)
		maxTextBytes int64
		want         error
	}{
		{"empty text", func(c *PreparedContext) { c.Text = ""; c.ContextHash = testHash("") }, 0, ErrContextInvalid},
		{"invalid utf8 text", func(c *PreparedContext) { c.Text = string([]byte{0xff, 0xfe}); c.ContextHash = testHash(c.Text) }, 0, ErrContextInvalid},
		{"nul in text", func(c *PreparedContext) { c.Text = "frozen\x00bytes"; c.ContextHash = testHash(c.Text) }, 0, ErrContextInvalid},
		{"hash mismatch", func(c *PreparedContext) { c.ContextHash = testHash("some other frozen context") }, 0, ErrContextInvalid},
		{"uppercase hash", func(c *PreparedContext) { c.ContextHash = strings.ToUpper(c.ContextHash) }, 0, ErrContextInvalid},
		{"empty hash", func(c *PreparedContext) { c.ContextHash = "" }, 0, ErrContextInvalid},
		{"empty project", func(c *PreparedContext) { c.ProjectID = "" }, 0, ErrContextInvalid},
		{"nul in project", func(c *PreparedContext) { c.ProjectID = "project\x00alpha" }, 0, ErrContextInvalid},
		{"empty operation", func(c *PreparedContext) { c.Operation = "" }, 0, ErrContextInvalid},
		{"empty status", func(c *PreparedContext) { c.Status = "" }, 0, ErrContextInvalid},
		{"negative draft", func(c *PreparedContext) { c.DraftID = -1 }, 0, ErrContextInvalid},
		{"draft above safe integer", func(c *PreparedContext) { c.DraftID = 1 << 53 }, 0, ErrContextInvalid},
		{"bad source hash", func(c *PreparedContext) { c.SourceHash = "not-a-hash" }, 0, ErrContextInvalid},
		{"text above the store cap", func(c *PreparedContext) {}, 16, ErrTextTooLarge},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock, StoreOptions{MaxTextBytes: testCase.maxTextBytes})
			context := testPreparedContext()
			testCase.adjust(&context)
			if _, err := store.Prepare(testBinding("alice"), context); err != nil {
				requireCode(t, err, testCase.want)
			} else {
				t.Fatal("Prepare accepted an invalid context")
			}
			if count := store.ActiveCount(); count != 0 {
				t.Fatalf("ActiveCount = %d, want 0", count)
			}
		})
	}
}

func TestUnknownHandleIsRefused(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	binding := testBinding("alice")
	for _, id := range []string{"", "not-a-uuid", strings.Repeat("a", 129), "00000000-0000-4000-8000-000000000000"} {
		if _, err := store.Peek(id, binding); err != nil {
			requireCode(t, err, ErrHandleUnknown)
		} else {
			t.Fatal("Peek of an unknown handle succeeded")
		}
		if _, err := store.Consume(id, testPreparedContext().ContextHash, binding); err != nil {
			requireCode(t, err, ErrHandleUnknown)
		} else {
			t.Fatal("Consume of an unknown handle succeeded")
		}
	}
}

func TestPrepareMintsDistinctIDs(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	binding := testBinding("alice")
	first := mustPrepare(t, store, binding, testPreparedContext())
	second := mustPrepare(t, store, binding, testPreparedContext())
	if first == second {
		t.Fatal("two prepares for one context returned the same id")
	}
	for _, id := range []string{first, second} {
		if _, err := store.Peek(id, binding); err != nil {
			t.Fatalf("Peek: %v", err)
		}
	}
	if count := store.ActiveCount(); count != 2 {
		t.Fatalf("ActiveCount = %d, want 2", count)
	}
	// The id carries nothing: no project, caller, generation or hash fragment.
	for _, fragment := range []string{"project-alpha", "alice", "run-1", "review-chapter", testPreparedContext().ContextHash} {
		if strings.Contains(first, fragment) {
			t.Fatalf("handle id contains a caller-selected fragment")
		}
	}
}

func TestReturnedContextIsACopy(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{})
	binding := testBinding("alice")
	stored := testPreparedContext()
	id := mustPrepare(t, store, binding, stored)

	returned, err := store.Peek(id, binding)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	returned.Text = "tampered"
	returned.ContextHash = testHash("tampered")
	returned.ProjectID = "other-project"

	stored.Text = "tampered too"

	again, err := store.Peek(id, binding)
	if err != nil {
		t.Fatalf("second Peek: %v", err)
	}
	if again != testPreparedContext() {
		t.Fatal("a handed-out copy changed the stored frozen bytes")
	}
	if again.ContextHash != testHash(again.Text) {
		t.Fatal("stored hash no longer matches the stored text")
	}
}

func TestNewStoreValidation(t *testing.T) {
	cases := []struct {
		name    string
		key     []byte
		options StoreOptions
		want    error
	}{
		{"short key", []byte("0123456789abcdef0123456789abcde"), StoreOptions{}, ErrServerKeyInvalid},
		{"nil key", nil, StoreOptions{}, ErrServerKeyInvalid},
		{"defaults", []byte(testKey), StoreOptions{}, nil},
		{"negative ttl", []byte(testKey), StoreOptions{TTL: -time.Second}, ErrOptionsInvalid},
		{"ttl above the reference cap", []byte(testKey), StoreOptions{TTL: 11 * time.Minute}, ErrOptionsInvalid},
		{"negative max active", []byte(testKey), StoreOptions{MaxActive: -1}, ErrOptionsInvalid},
		{"max active above cap", []byte(testKey), StoreOptions{MaxActive: 101}, ErrOptionsInvalid},
		{"negative per caller", []byte(testKey), StoreOptions{MaxPerCaller: -1}, ErrOptionsInvalid},
		{"per caller above cap", []byte(testKey), StoreOptions{MaxPerCaller: 101}, ErrOptionsInvalid},
		{"negative text bytes", []byte(testKey), StoreOptions{MaxTextBytes: -1}, ErrOptionsInvalid},
		{"text bytes above cap", []byte(testKey), StoreOptions{MaxTextBytes: (64 << 20) + 1}, ErrOptionsInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := NewStore(testCase.key, testCase.options)
			if testCase.want != nil {
				if err != nil {
					requireCode(t, err, testCase.want)
				} else {
					t.Fatal("NewStore accepted invalid options")
				}
				if store != nil {
					t.Fatal("NewStore returned a store with an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			if store == nil {
				t.Fatal("NewStore returned nil without an error")
			}
		})
	}
}

func TestStoreLimits(t *testing.T) {
	cases := []struct {
		name    string
		options StoreOptions
		run     func(t *testing.T, store *Store, clock *fakeClock)
	}{
		{"total entries", StoreOptions{MaxActive: 1}, func(t *testing.T, store *Store, clock *fakeClock) {
			mustPrepare(t, store, testBinding("alice"), testPreparedContext())
			if _, err := store.Prepare(testBinding("bob"), testPreparedContext()); err != nil {
				requireCode(t, err, ErrLimit)
			} else {
				t.Fatal("Prepare above MaxActive succeeded")
			}
		}},
		{"entries per caller", StoreOptions{MaxActive: 8, MaxPerCaller: 1}, func(t *testing.T, store *Store, clock *fakeClock) {
			mustPrepare(t, store, testBinding("alice"), testPreparedContext())
			if _, err := store.Prepare(testBinding("alice"), testPreparedContext()); err != nil {
				requireCode(t, err, ErrLimit)
			} else {
				t.Fatal("Prepare above MaxPerCaller succeeded")
			}
			// Another caller is unaffected by one caller's own cap.
			mustPrepare(t, store, testBinding("bob"), testPreparedContext())
		}},
		{"retained bytes", StoreOptions{MaxTextBytes: 140}, func(t *testing.T, store *Store, clock *fakeClock) {
			if len(testContextText()) >= 140 {
				t.Fatalf("fixture text is %d bytes, the test expects it below 140", len(testContextText()))
			}
			mustPrepare(t, store, testBinding("alice"), testPreparedContext())
			if _, err := store.Prepare(testBinding("bob"), testPreparedContext()); err != nil {
				requireCode(t, err, ErrTextTooLarge)
			} else {
				t.Fatal("Prepare above MaxTextBytes succeeded")
			}
		}},
		{"expiry frees capacity", StoreOptions{MaxActive: 1}, func(t *testing.T, store *Store, clock *fakeClock) {
			mustPrepare(t, store, testBinding("alice"), testPreparedContext())
			clock.Advance(10 * time.Minute)
			mustPrepare(t, store, testBinding("bob"), testPreparedContext())
			if count := store.ActiveCount(); count != 1 {
				t.Fatalf("ActiveCount = %d, want 1", count)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clock := newFakeClock()
			store := newTestStore(t, clock, testCase.options)
			testCase.run(t, store, clock)
		})
	}
}

func TestIDFactorySeam(t *testing.T) {
	t.Run("factory error", func(t *testing.T) {
		clock := newFakeClock()
		want := errors.New("entropy unavailable")
		store := newTestStore(t, clock, StoreOptions{ID: func() (string, error) { return "", want }})
		if _, err := store.Prepare(testBinding("alice"), testPreparedContext()); err != nil {
			if !errors.Is(err, want) {
				t.Fatalf("Prepare = %v, want %v", err, want)
			}
		} else {
			t.Fatal("Prepare succeeded with a failing id factory")
		}
		if count := store.ActiveCount(); count != 0 {
			t.Fatalf("ActiveCount = %d, want 0", count)
		}
	})

	t.Run("empty id", func(t *testing.T) {
		clock := newFakeClock()
		store := newTestStore(t, clock, StoreOptions{ID: func() (string, error) { return "", nil }})
		if _, err := store.Prepare(testBinding("alice"), testPreparedContext()); err != nil {
			requireCode(t, err, ErrOptionsInvalid)
		} else {
			t.Fatal("Prepare accepted an empty id")
		}
	})

	t.Run("duplicate id does not overwrite a live handle", func(t *testing.T) {
		clock := newFakeClock()
		store := newTestStore(t, clock, StoreOptions{ID: func() (string, error) { return "fixed-handle", nil }})
		alice := testBinding("alice")
		if _, err := store.Prepare(alice, testPreparedContext()); err != nil {
			t.Fatalf("first Prepare: %v", err)
		}
		if _, err := store.Prepare(testBinding("bob"), testPreparedContext()); err != nil {
			requireCode(t, err, ErrOptionsInvalid)
		} else {
			t.Fatal("Prepare overwrote a live handle")
		}
		if _, err := store.Peek("fixed-handle", alice); err != nil {
			t.Fatalf("Peek after refused overwrite: %v", err)
		}
	})

	t.Run("revoke during mint fails closed", func(t *testing.T) {
		clock := newFakeClock()
		var store *Store
		store = newTestStore(t, clock, StoreOptions{ID: func() (string, error) {
			// The factory runs outside the mutex, so this is the exact race a
			// page reload causes: a revoke lands after the epoch was sampled and
			// before the entry is published.
			store.RevokeCaller("alice")
			return "raced-handle", nil
		}})
		if _, err := store.Prepare(testBinding("alice"), testPreparedContext()); err != nil {
			requireCode(t, err, ErrHandleRevoked)
		} else {
			t.Fatal("Prepare published a handle a revoke had already passed")
		}
		if count := store.ActiveCount(); count != 0 {
			t.Fatalf("ActiveCount = %d, want 0", count)
		}
	})
}

func TestNilStoreIsRefused(t *testing.T) {
	var store *Store
	if _, err := store.Prepare(testBinding("alice"), testPreparedContext()); err != nil {
		requireCode(t, err, ErrStoreUnavailable)
	} else {
		t.Fatal("nil Store accepted Prepare")
	}
	if _, err := store.Peek("handle", testBinding("alice")); err != nil {
		requireCode(t, err, ErrStoreUnavailable)
	} else {
		t.Fatal("nil Store accepted Peek")
	}
	if _, err := store.Consume("handle", testPreparedContext().ContextHash, testBinding("alice")); err != nil {
		requireCode(t, err, ErrStoreUnavailable)
	} else {
		t.Fatal("nil Store accepted Consume")
	}
	store.RevokeCaller("alice")
	if count := store.ActiveCount(); count != 0 {
		t.Fatalf("nil Store ActiveCount = %d, want 0", count)
	}
}

func TestConcurrentPrepareAndConsume(t *testing.T) {
	clock := newFakeClock()
	store := newTestStore(t, clock, StoreOptions{MaxActive: 32, MaxPerCaller: 4})
	failures := make(chan error, 4)
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		caller := "worker-" + strconv.Itoa(worker)
		group.Add(1)
		go func() {
			defer group.Done()
			binding := testBinding(caller)
			for round := 0; round < 20; round++ {
				id, err := store.Prepare(binding, testPreparedContext())
				if err != nil {
					failures <- err
					return
				}
				if _, err := store.Peek(id, binding); err != nil {
					failures <- err
					return
				}
				if _, err := store.Consume(id, testPreparedContext().ContextHash, binding); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent prepare/consume: %v", err)
	}
	if count := store.ActiveCount(); count != 0 {
		t.Fatalf("ActiveCount = %d, want 0", count)
	}
}
