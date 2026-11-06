package librarypath

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// grantsFixture is the token store on a fake clock. The clock is a plain closure
// over a variable, so a test advances time by assigning to it; the registry never
// calls time.Now once Now is injected.
type grantsFixture struct {
	roots    Roots
	registry *Registry
	binding  Binding
	target   Target
	readOnly Target
	now      *time.Time
}

func newGrantsFixture(t *testing.T) grantsFixture {
	t.Helper()
	fixture := newPathFixture(t)
	at := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	clock := &at
	registry, err := NewRegistry(bytes.Repeat([]byte{7}, 32), RegistryOptions{Now: func() time.Time {
		return *clock
	}})
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	target, err := fixture.roots.Resolve(LibraryRootID, "exports/chapters")
	if err != nil {
		t.Fatalf("resolve the grant target: %v", err)
	}
	readOnly, err := fixture.roots.Resolve(LibraryRootID, "exports/chapters/one.md")
	if err != nil {
		t.Fatalf("resolve the read-only target: %v", err)
	}
	return grantsFixture{
		roots:    fixture.roots,
		registry: registry,
		binding:  Binding{SessionID: "session-1", ClientID: "tab-1"},
		target:   target,
		readOnly: readOnly,
		now:      clock,
	}
}

// TestRegistryMintsAndRedeemsOnePath is the acceptance case and the usage record:
// a token resolves to exactly the target it was minted for, the operation set it
// carries is what it may do, and every redeem spends one use of a single-writer-
// safe budget.
func TestRegistryMintsAndRedeemsOnePath(t *testing.T) {
	fixture := newGrantsFixture(t)
	token, err := fixture.registry.Mint(fixture.binding, fixture.target,
		OpWrite|OpMkdir, MintOptions{TTL: time.Minute, MaxUses: 2})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	grant, err := fixture.registry.Redeem(token, fixture.binding, OpMkdir)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if grant.Target != fixture.target {
		t.Fatalf("Redeem answered a different target than the minted one")
	}
	if grant.Ops != OpWrite|OpMkdir || !grant.Ops.Permits(OpMkdir) || !grant.Ops.Permits(OpWrite) {
		t.Fatalf("Redeem answered the operation set %v", grant.Ops)
	}
	if grant.Remaining != 1 {
		t.Fatalf("Redeem left %d uses; want 1", grant.Remaining)
	}
	if _, err := fixture.registry.Redeem(token, fixture.binding, OpWrite); err != nil {
		t.Fatalf("second Redeem: %v", err)
	}
	if _, err := fixture.registry.Redeem(token, fixture.binding, OpWrite); !errors.Is(err, ErrGrantExhausted) {
		t.Fatalf("third Redeem = %v; want %v", err, ErrGrantExhausted)
	}
	if active := fixture.registry.ActiveCount(); active != 1 {
		t.Fatalf("ActiveCount = %d; want 1", active)
	}
}

// TestRegistryRefusesEveryWrongUse is the refusal table: another caller, an
// expired token, a revoked token, an operation the token does not carry, and a
// token that was never minted. Each case is a fresh registry, so one refusal
// cannot mask another.
func TestRegistryRefusesEveryWrongUse(t *testing.T) {
	const readOps = OpRead
	cases := []struct {
		name    string
		prepare func(t *testing.T, fixture grantsFixture) (string, Binding, Op)
		want    error
	}{
		{
			name: "other-caller",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				return token, Binding{SessionID: "session-2", ClientID: "tab-1"}, OpRead
			},
			want: ErrCallerMismatch,
		},
		{
			name: "other-tab",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				return token, Binding{SessionID: "session-1", ClientID: "tab-2"}, OpRead
			},
			want: ErrCallerMismatch,
		},
		{
			name: "expired",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{TTL: time.Millisecond})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				*fixture.now = fixture.now.Add(2 * time.Millisecond)
				return token, fixture.binding, OpRead
			},
			want: ErrGrantExpired,
		},
		{
			name: "revoked-token",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				if !fixture.registry.Revoke(token) {
					t.Fatal("Revoke reported nothing to revoke")
				}
				return token, fixture.binding, OpRead
			},
			want: ErrGrantRevoked,
		},
		{
			name: "revoked-caller",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				if dropped := fixture.registry.RevokeCaller(fixture.binding); dropped != 1 {
					t.Fatalf("RevokeCaller dropped %d tokens; want 1", dropped)
				}
				return token, fixture.binding, OpRead
			},
			want: ErrGrantUnknown,
		},
		{
			name: "operation-denied",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.readOnly, OpRead, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				return token, fixture.binding, OpMkdir
			},
			want: ErrGrantOperationDenied,
		},
		{
			name: "write-without-create-is-a-write",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, OpWrite, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				return token, fixture.binding, OpWrite
			},
			want: nil,
		},
		{
			name: "unknown-token",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				return "not-a-minted-token", fixture.binding, OpRead
			},
			want: ErrGrantUnknown,
		},
		{
			name: "empty-token",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				return "", fixture.binding, OpRead
			},
			want: ErrGrantUnknown,
		},
		{
			name: "empty-operation",
			prepare: func(t *testing.T, fixture grantsFixture) (string, Binding, Op) {
				token, err := fixture.registry.Mint(fixture.binding, fixture.target, readOps, MintOptions{})
				if err != nil {
					t.Fatalf("Mint: %v", err)
				}
				return token, fixture.binding, 0
			},
			want: ErrGrantOperationDenied,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newGrantsFixture(t)
			token, binding, required := testCase.prepare(t, fixture)
			_, err := fixture.registry.Redeem(token, binding, required)
			if testCase.want == nil {
				if err != nil {
					t.Fatalf("Redeem(%v) = %v; want a grant", testCase.name, err)
				}
				return
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("Redeem(%v) = %v; want %v", testCase.name, err, testCase.want)
			}
		})
	}
}

// TestRegistryPeekCostsNothingAndSpendCostsOne is the two-phase rule a refused path
// depends on: Peek answers the target over and over without touching the budget, so
// a caller who mistypes a path is not locked out, and Spend is what takes the use -
// with the budget re-read under the same lock, so a spend that arrives after the
// last use was taken is refused rather than double counted.
func TestRegistryPeekCostsNothingAndSpendCostsOne(t *testing.T) {
	fixture := newGrantsFixture(t)
	token, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead, MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		grant, err := fixture.registry.Peek(token, fixture.binding, OpRead)
		if err != nil {
			t.Fatalf("Peek %d: %v", attempt, err)
		}
		if grant.Target != fixture.target || grant.Remaining != 1 {
			t.Fatalf("Peek %d answered %v remaining=%d; want the minted target and 1",
				attempt, grant.Target.Relative, grant.Remaining)
		}
	}
	// Three peeks and the single use is still there.
	spent, err := fixture.registry.Spend(token, fixture.binding, OpRead)
	if err != nil {
		t.Fatalf("Spend after three peeks: %v", err)
	}
	if spent.Remaining != 0 {
		t.Fatalf("Spend left %d uses; want 0", spent.Remaining)
	}
	if _, err := fixture.registry.Peek(token, fixture.binding, OpRead); !errors.Is(err, ErrGrantExhausted) {
		t.Fatalf("Peek after the last use = %v; want %v", err, ErrGrantExhausted)
	}
	if _, err := fixture.registry.Spend(token, fixture.binding, OpRead); !errors.Is(err, ErrGrantExhausted) {
		t.Fatalf("Spend after the last use = %v; want %v", err, ErrGrantExhausted)
	}
	// Peek is not a way around the caller, the operation or the expiry checks
	// either: it runs exactly the same admission policy.
	if _, err := fixture.registry.Peek(token, Binding{SessionID: "session-2", ClientID: "tab-1"}, OpRead); !errors.Is(err, ErrCallerMismatch) {
		t.Fatalf("Peek from another caller = %v; want %v", err, ErrCallerMismatch)
	}
	if _, err := fixture.registry.Peek("not-a-minted-token", fixture.binding, OpRead); !errors.Is(err, ErrGrantUnknown) {
		t.Fatalf("Peek of an unknown token = %v; want %v", err, ErrGrantUnknown)
	}
	if _, err := fixture.registry.Peek(token, fixture.binding, OpWrite); !errors.Is(err, ErrGrantOperationDenied) {
		t.Fatalf("Peek of an operation the grant lacks = %v; want %v", err, ErrGrantOperationDenied)
	}
	*fixture.now = fixture.now.Add(defaultGrantTTL + time.Second)
	if _, err := fixture.registry.Peek(token, fixture.binding, OpRead); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("Peek of an expired token = %v; want %v", err, ErrGrantExpired)
	}
}

// TestRegistryRefusesATamperedRecord is the MAC's own test. A record that was put
// into the map by anything other than Mint no longer matches the keyed digest, so
// the store drops it and answers "unknown" rather than honouring a budget the
// caller just widened.
func TestRegistryRefusesATamperedRecord(t *testing.T) {
	fixture := newGrantsFixture(t)
	token, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead, MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	fixture.registry.mu.Lock()
	item := fixture.registry.entries[token]
	item.remaining = 4096
	fixture.registry.entries[token] = item
	fixture.registry.mu.Unlock()
	if _, err := fixture.registry.Redeem(token, fixture.binding, OpRead); !errors.Is(err, ErrGrantUnknown) {
		t.Fatalf("Redeem on a tampered record = %v; want %v", err, ErrGrantUnknown)
	}
	if active := fixture.registry.ActiveCount(); active != 0 {
		t.Fatalf("ActiveCount after a tampered record = %d; want 0", active)
	}
}

// TestRegistryRefusesUnusableInput is the validation table on the mint side: a
// short key, unusable options, an empty binding, a target the allowlist could not
// have produced, an empty operation set, and an exhausted per-caller cap.
func TestRegistryRefusesUnusableInput(t *testing.T) {
	fixture := newGrantsFixture(t)
	if _, err := NewRegistry([]byte("short"), RegistryOptions{}); !errors.Is(err, ErrSecretInvalid) {
		t.Fatalf("NewRegistry with a short key = %v; want %v", err, ErrSecretInvalid)
	}
	for _, options := range []RegistryOptions{
		{TTL: time.Second, MaxActive: -1},
		{TTL: time.Second, MaxPerCaller: limitMaxPerCaller + 1},
		{TTL: maxGrantTTL + time.Second},
	} {
		if _, err := NewRegistry(bytes.Repeat([]byte{1}, 32), options); !errors.Is(err, ErrOptionsInvalid) {
			t.Fatalf("NewRegistry(%+v) = %v; want %v", options, err, ErrOptionsInvalid)
		}
	}
	var absent *Registry
	if _, err := absent.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("Mint on a nil registry = %v; want %v", err, ErrRegistryUnavailable)
	}
	if _, err := absent.Redeem("token", fixture.binding, OpRead); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("Redeem on a nil registry = %v; want %v", err, ErrRegistryUnavailable)
	}
	if absent.Revoke("token") || absent.ActiveCount() != 0 {
		t.Fatal("a nil registry answered a revocation or an active count")
	}
	if _, err := fixture.registry.Mint(Binding{}, fixture.target, OpRead, MintOptions{}); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("Mint with an empty binding = %v; want %v", err, ErrBindingInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, Target{}, OpRead, MintOptions{}); !errors.Is(err, ErrTargetInvalid) {
		t.Fatalf("Mint with an empty target = %v; want %v", err, ErrTargetInvalid)
	}
	forged := fixture.target
	forged.Path = filepath.Join(fixture.target.Root, "somewhere", "else")
	if _, err := fixture.registry.Mint(fixture.binding, forged, OpRead, MintOptions{}); !errors.Is(err, ErrTargetInvalid) {
		t.Fatalf("Mint with a forged target = %v; want %v", err, ErrTargetInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, fixture.target, 0, MintOptions{}); !errors.Is(err, ErrTargetInvalid) {
		t.Fatalf("Mint with no operations = %v; want %v", err, ErrTargetInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead|Op(8), MintOptions{}); !errors.Is(err, ErrTargetInvalid) {
		t.Fatalf("Mint with an unknown operation = %v; want %v", err, ErrTargetInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead, MintOptions{TTL: -time.Second}); !errors.Is(err, ErrOptionsInvalid) {
		t.Fatalf("Mint with a negative TTL = %v; want %v", err, ErrOptionsInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead,
		MintOptions{TTL: maxGrantTTL + time.Second}); !errors.Is(err, ErrOptionsInvalid) {
		t.Fatalf("Mint above the TTL ceiling = %v; want %v", err, ErrOptionsInvalid)
	}
	if _, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead,
		MintOptions{MaxUses: limitMaxUses + 1}); !errors.Is(err, ErrOptionsInvalid) {
		t.Fatalf("Mint above the usage ceiling = %v; want %v", err, ErrOptionsInvalid)
	}

	capped, err := NewRegistry(bytes.Repeat([]byte{2}, 32), RegistryOptions{
		Now:          func() time.Time { return *fixture.now },
		MaxActive:    1,
		MaxPerCaller: 1,
	})
	if err != nil {
		t.Fatalf("build the capped registry: %v", err)
	}
	if _, err := capped.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); err != nil {
		t.Fatalf("first Mint under the cap: %v", err)
	}
	if _, err := capped.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); !errors.Is(err, ErrGrantLimit) {
		t.Fatalf("Mint above the per-caller cap = %v; want %v", err, ErrGrantLimit)
	}
}

// TestRegistryRefusesARepeatedToken is the token factory seam: a factory that
// hands back a token that is already live would give one caller another caller's
// path authority, so the mint refuses instead of overwriting it.
func TestRegistryRefusesARepeatedToken(t *testing.T) {
	fixture := newGrantsFixture(t)
	repeated, err := NewRegistry(bytes.Repeat([]byte{3}, 32), RegistryOptions{
		Now:   func() time.Time { return *fixture.now },
		Token: func() (string, error) { return "one-token", nil },
	})
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	if _, err := repeated.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); err != nil {
		t.Fatalf("first Mint: %v", err)
	}
	if _, err := repeated.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); !errors.Is(err, ErrOptionsInvalid) {
		t.Fatalf("repeated token Mint = %v; want %v", err, ErrOptionsInvalid)
	}
	unusable, err := NewRegistry(bytes.Repeat([]byte{4}, 32), RegistryOptions{
		Now:   func() time.Time { return *fixture.now },
		Token: func() (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	if _, err := unusable.Mint(fixture.binding, fixture.target, OpRead, MintOptions{}); !errors.Is(err, ErrOptionsInvalid) {
		t.Fatalf("empty token Mint = %v; want %v", err, ErrOptionsInvalid)
	}
}

// TestRegistrySpendsOneUsePerCallerUnderConcurrency is the single-writer-safe
// usage record under load: sixteen concurrent redeems of a one-use token spend
// exactly one, and the other fifteen are told the budget is gone.
func TestRegistrySpendsOneUsePerCallerUnderConcurrency(t *testing.T) {
	fixture := newGrantsFixture(t)
	token, err := fixture.registry.Mint(fixture.binding, fixture.target, OpRead, MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	granted, refused := 0, 0
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := fixture.registry.Redeem(token, fixture.binding, OpRead)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				granted++
				return
			}
			if !errors.Is(err, ErrGrantExhausted) {
				refused = -1000
				return
			}
			refused++
		}()
	}
	wait.Wait()
	if granted != 1 || refused != 15 {
		t.Fatalf("concurrent redeems granted=%d refused=%d; want 1 and 15", granted, refused)
	}
}
