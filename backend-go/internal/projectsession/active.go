package projectsession

import (
	"context"
	"errors"
	"sync"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrStaleOpen = errors.New("PROJECT_OPEN_STALE")
var ErrActiveClosed = errors.New("PROJECT_ACTIVE_COORDINATOR_CLOSED")
var ErrActiveCleanup = errors.New("PROJECT_ACTIVE_CLEANUP_UNCONFIRMED")

// Admitted is supplied by a future durable project opener. Its Context must
// describe the actually admitted database; Verify checks that it is still
// usable under the held legacy writer lock. Close releases that database.
// Neither a path nor a browser request may construct an Admitted capability.
type Admitted interface {
	Context() Context
	Verify(context.Context) error
	Close() error
}

// StageActive must return a fully admitted candidate while the legacy lock is
// held. On error it must clean up its own partial candidate. A successful
// candidate is owned by ActiveCoordinator, even if its request becomes stale.
type StageActive func(context.Context, *writerlock.Lock) (Admitted, error)

type activeSlot struct {
	handle             Admitted
	key                string
	latest             uint64
	admittedGeneration uint64
	active             int
	idle               chan struct{}
	switching          bool
	committing         bool
	// tabs records the TAB IDENTITIES that have completed their own Open against
	// this slot and the last lease each of those tabs was shown. The slot's lease
	// key may be account-level (so every tab of one author shares one handle and
	// one project context), but the handle is only lent to a tab that opened the
	// project itself: a browser tab that never opened it holds no session context
	// and stays refused exactly as it was when the lease was keyed per tab. A lane
	// that never records a tab here (its owners leave Owner.Tab empty) is not
	// fenced by this map at all.
	tabs map[string]string
}

// admitTab records one completed Open for this tab together with the lease this
// tab was shown. An owner that carries no tab identity is not tracked: such a
// lane keeps its previous behaviour.
func (s *activeSlot) admitTab(tab, lease string) {
	if s == nil || tab == "" {
		return
	}
	if s.tabs == nil {
		s.tabs = make(map[string]string)
	}
	s.tabs[tab] = lease
}

// tabLease returns the lease this tab was last shown. Only a tracked tab has an
// own view; an untracked lane has none and keeps using the slot's lease.
func (s *activeSlot) tabLease(tab string) (string, bool) {
	if s == nil || tab == "" {
		return "", false
	}
	lease, ok := s.tabs[tab]
	return lease, ok && lease != ""
}

// admitsTab reports whether one borrow may use this slot's handle. It is the
// per-tab half of the fence: a slot that has recorded tabs serves only those
// tabs, while a caller (or a whole lane) that carries no tab identity is left
// unfenced, so a lane which never tracks tabs behaves as it always did.
func (s *activeSlot) admitsTab(tab string) bool {
	if s == nil || tab == "" || len(s.tabs) == 0 {
		return true
	}
	_, admitted := s.tabs[tab]
	return admitted
}

// ActiveCoordinator owns one continuously held legacy writer lock. It only
// coordinates independently admitted handles: it does not open author data or
// assert database readiness. Keys must come from a trusted catalog/grant store.
// A single transition mutex serializes commit/close while Open registers its
// latest token before waiting for that mutex.
type ActiveCoordinator struct {
	mu         sync.Mutex
	transition sync.Mutex
	root       string
	lock       *writerlock.Lock
	slots      map[string]*activeSlot
	closed     bool
	poisoned   bool
}

// NewActiveCoordinator transfers ownership of held to the coordinator. It
// must not be closed independently after this call.
func NewActiveCoordinator(root string, held *writerlock.Lock) (*ActiveCoordinator, error) {
	if err := held.VerifyLegacy(root); err != nil {
		return nil, err
	}
	return &ActiveCoordinator{root: root, lock: held, slots: make(map[string]*activeSlot)}, nil
}

func sameContext(a, b Context) bool {
	return a.ProjectID == b.ProjectID && a.ProjectPath == b.ProjectPath && a.LeaseID == b.LeaseID && a.ProjectID != "" && a.ProjectPath != "" && a.LeaseID != ""
}

// sameProjectContext compares the stable project identity only. The account
// lease is shared by every tab of one author and rotates in place when any of
// them acknowledges a draft save; that rotation must not revoke a sibling tab's
// already-admitted project session. Callers that need the exact lease (commit,
// publish and open-generation paths) keep using sameContext.
func sameProjectContext(a, b Context) bool {
	return a.ProjectID != "" && a.ProjectPath != "" && a.ProjectID == b.ProjectID && a.ProjectPath == b.ProjectPath
}

// admitsFrozenContext is the Begin/Close fence. A lane that tracks tab
// identities (Owner.Tab non-empty) accepts the project context while requiring
// the lease this particular tab was last shown, so a sibling tab's in-place
// rotation does not revoke it and a lease this tab never held is still refused.
// A lane that does not track tabs keeps the exact lease comparison it always had.
func admitsFrozenContext(state *activeSlot, owner Owner, expected Context) bool {
	if state == nil || state.handle == nil {
		return false
	}
	if owner.Tab == "" {
		return sameContext(state.handle.Context(), expected)
	}
	if !sameProjectContext(state.handle.Context(), expected) {
		return false
	}
	lease, ok := state.tabLease(owner.Tab)
	return ok && lease == expected.LeaseID
}

func validActiveContext(value Context) bool {
	return value.ProjectID != "" && value.ProjectPath != "" && value.LeaseID != ""
}

func (c *ActiveCoordinator) slot(owner Owner) *activeSlot {
	state := c.slots[owner.key()]
	if state == nil {
		state = &activeSlot{}
		c.slots[owner.key()] = state
	}
	return state
}

// Open registers a new request generation before entering the transition
// queue. A failed or superseded candidate leaves the existing handle intact.
// Same-key refresh verifies and returns the existing admitted handle.
func (c *ActiveCoordinator) Open(ctx context.Context, owner Owner, key, requestToken string, stage StageActive) (Context, error) {
	if ctx == nil || !owner.valid() || key == "" || requestToken == "" || stage == nil {
		return Context{}, ErrContext
	}
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, ErrActiveClosed
	}
	state := c.slot(owner)
	if state.committing {
		c.mu.Unlock()
		return Context{}, ErrCommitInProgress
	}
	state.latest++
	request := state.latest
	c.mu.Unlock()

	c.transition.Lock()
	defer c.transition.Unlock()
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, ErrActiveClosed
	}
	state = c.slot(owner)
	if state.latest != request {
		c.mu.Unlock()
		return Context{}, ErrStaleOpen
	}
	current := state.handle
	refresh := current != nil && state.key == key
	c.mu.Unlock()
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return Context{}, err
	}
	if refresh {
		if err := current.Verify(ctx); err != nil {
			return Context{}, err
		}
		var refreshed Context
		c.mu.Lock()
		stale := c.closed || state.latest != request
		if !stale {
			refreshed = current.Context()
			state.admittedGeneration = request
			// A sibling tab of the same account adopts the live lease here, and
			// records the exact value it is about to receive so only IT may borrow
			// the shared handle and Current never reports an unseen lease.
			state.admitTab(owner.Tab, refreshed.LeaseID)
		}
		c.mu.Unlock()
		if stale {
			return Context{}, ErrStaleOpen
		}
		return refreshed, nil
	}

	candidate, err := stage(ctx, c.lock)
	if err != nil {
		return Context{}, err
	}
	if candidate == nil {
		return Context{}, ErrContext
	}
	adopted := false
	defer func() {
		if !adopted {
			if closeErr := candidate.Close(); closeErr != nil {
				// An unclosed DB may still hold source authority. Never release the
				// shared writer lock after losing track of such a candidate.
				c.mu.Lock()
				c.closed = true
				c.poisoned = true
				c.mu.Unlock()
			}
		}
	}()
	identity := candidate.Context()
	if !validActiveContext(identity) {
		return Context{}, ErrContext
	}
	if err := candidate.Verify(ctx); err != nil {
		return Context{}, err
	}
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return Context{}, err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, ErrActiveClosed
	}
	if state.latest != request {
		c.mu.Unlock()
		return Context{}, ErrStaleOpen
	}
	for ownerKey, other := range c.slots {
		if ownerKey != owner.key() && other.handle != nil && other.handle.Context().ProjectPath == identity.ProjectPath {
			c.mu.Unlock()
			return Context{}, novelruntime.ErrOwned
		}
	}
	state.switching = true
	for state.active > 0 {
		idle := state.idle
		c.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			c.mu.Lock()
			state.switching = false
			c.mu.Unlock()
			return Context{}, ctx.Err()
		}
		c.mu.Lock()
	}
	if c.closed || state.latest != request {
		state.switching = false
		c.mu.Unlock()
		return Context{}, ErrStaleOpen
	}
	previous := state.handle
	state.handle = candidate
	state.key = key
	state.admittedGeneration = request
	// A new handle (including a project switch) starts a new tab generation:
	// tabs admitted to the previous handle must Open this one before borrowing it.
	state.tabs = nil
	state.admitTab(owner.Tab, identity.LeaseID)
	adopted = true
	c.mu.Unlock()
	if previous != nil {
		if err := previous.Close(); err != nil {
			c.mu.Lock()
			state.handle = nil
			state.key = ""
			state.admittedGeneration = 0
			state.tabs = nil
			state.switching = false
			c.closed = true
			c.poisoned = true
			c.mu.Unlock()
			return Context{}, errors.Join(err, candidate.Close())
		}
	}
	c.mu.Lock()
	state.switching = false
	c.mu.Unlock()
	return identity, nil
}

// Begin fences a complete repository operation using the frozen project
// context. A sibling tab's acknowledged save may rotate the shared account lease
// in place; that does not revoke this tab. A switching or closing slot cannot
// issue new operations. done is idempotent.
func (c *ActiveCoordinator) Begin(ctx context.Context, owner Owner, expected Context) (Admitted, func(), error) {
	if ctx == nil || !owner.valid() {
		return nil, nil, ErrContext
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, ErrActiveClosed
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return nil, nil, err
	}
	state := c.slots[owner.key()]
	if !admitsFrozenContext(state, owner, expected) {
		return nil, nil, novelruntime.ErrNotOwned
	}
	// The account lease is shared by every tab of one author, but the handle is
	// only lent to a tab that opened this project itself: a tab that never opened
	// it holds no session context of its own and is refused exactly as before.
	if !state.admitsTab(owner.Tab) {
		return nil, nil, novelruntime.ErrNotOwned
	}
	if state.switching {
		return nil, nil, novelruntime.ErrBusy
	}
	if state.active == 0 {
		state.idle = make(chan struct{})
	}
	state.active++
	var once sync.Once
	return state.handle, func() {
		once.Do(func() {
			c.mu.Lock()
			state.active--
			if state.active == 0 {
				close(state.idle)
				state.idle = nil
			}
			c.mu.Unlock()
		})
	}, nil
}

// Current reports the actually admitted handle for one exact browser owner.
// For a tab-tracked lane it returns that tab's own last-seen lease: a sibling
// tab's acknowledged save may rotate the shared lease in place, and advertising
// the newer value would make this tab look stale for a context it never held.
// The transition lock prevents a concurrent switch or close while Verify runs;
// a mere slot entry is never treated as database readiness.
func (c *ActiveCoordinator) Current(ctx context.Context, owner Owner) (Context, bool, error) {
	if c == nil || ctx == nil || !owner.valid() {
		return Context{}, false, ErrContext
	}
	if err := ctx.Err(); err != nil {
		return Context{}, false, err
	}
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, false, ErrActiveClosed
	}
	state := c.slots[owner.key()]
	if state == nil || state.handle == nil {
		c.mu.Unlock()
		return Context{}, false, nil
	}
	// A sibling tab that never opened the project reports no live context, exactly
	// as it did when the lease was keyed per tab and it had no slot at all.
	if !state.admitsTab(owner.Tab) {
		c.mu.Unlock()
		return Context{}, false, nil
	}
	handle := state.handle
	current := handle.Context()
	// Report this tab's own view. A sibling tab's acknowledged save rotates the
	// slot lease in place; showing that lease here would make the caller look
	// stale for a context it never held.
	if lease, ok := state.tabLease(owner.Tab); ok {
		current.LeaseID = lease
	}
	c.mu.Unlock()
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return Context{}, false, err
	}
	if err := handle.Verify(ctx); err != nil {
		return Context{}, false, err
	}
	return current, true, nil
}

// CurrentSession is the session-only half of Current: the slot, the tab fence
// and the retained writer lock, WITHOUT re-hashing the author database digest.
// The digest legitimately changes while a generation dispatches (the lane writes
// its own ledger rows), so a full Current would answer AUTHOR_WRITE_STALE for the
// lane's own writes. Entry points keep using Begin, which verifies the digest
// before any write, so nothing is widened.
func (c *ActiveCoordinator) CurrentSession(ctx context.Context, owner Owner) (Context, bool, error) {
	if c == nil || ctx == nil || !owner.valid() {
		return Context{}, false, ErrContext
	}
	if err := ctx.Err(); err != nil {
		return Context{}, false, err
	}
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, false, ErrActiveClosed
	}
	state := c.slots[owner.key()]
	if state == nil || state.handle == nil {
		c.mu.Unlock()
		return Context{}, false, nil
	}
	if !state.admitsTab(owner.Tab) {
		c.mu.Unlock()
		return Context{}, false, nil
	}
	current := state.handle.Context()
	if lease, ok := state.tabLease(owner.Tab); ok {
		current.LeaseID = lease
	}
	c.mu.Unlock()
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return Context{}, false, err
	}
	return current, true, nil
}

// AdoptCommittedLease records the fresh lease one tab received from an
// acknowledged in-place draft commit. The admitted handle has already adopted
// next.LeaseID; this method only updates the calling tab's own view so Current
// keeps reporting a lease that tab actually saw. A sibling's old lease stays
// valid for Begin/Close because those compare the project context.
func (c *ActiveCoordinator) AdoptCommittedLease(owner Owner, expected, next Context) error {
	if c == nil || !owner.valid() || !validActiveContext(next) ||
		!sameProjectContext(expected, next) || next.LeaseID == expected.LeaseID {
		return ErrContext
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrActiveClosed
	}
	state := c.slots[owner.key()]
	if state == nil || state.handle == nil || !sameContext(state.handle.Context(), next) || !state.admitsTab(owner.Tab) {
		return novelruntime.ErrNotOwned
	}
	state.admitTab(owner.Tab, next.LeaseID)
	return nil
}

// CloseProject first fences new calls, then waits for all project-context calls
// to finish. A tab that opened the project may close it with an older lease after
// a sibling save rotated the shared one. Cancellation restores the old handle.
func (c *ActiveCoordinator) CloseProject(ctx context.Context, owner Owner, expected Context) error {
	if ctx == nil || !owner.valid() {
		return ErrContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrActiveClosed
	}
	state := c.slots[owner.key()]
	if !admitsFrozenContext(state, owner, expected) {
		c.mu.Unlock()
		return novelruntime.ErrNotOwned
	}
	// Closing is a project operation like any other: a tab that never opened this
	// project cannot close the account's shared handle out from under it.
	if !state.admitsTab(owner.Tab) {
		c.mu.Unlock()
		return novelruntime.ErrNotOwned
	}
	state.switching = true
	for state.active > 0 {
		idle := state.idle
		c.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			c.mu.Lock()
			state.switching = false
			c.mu.Unlock()
			return ctx.Err()
		}
		c.mu.Lock()
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		state.switching = false
		c.mu.Unlock()
		return err
	}
	handle := state.handle
	state.handle = nil
	state.key = ""
	state.switching = false
	delete(c.slots, owner.key())
	c.mu.Unlock()
	if err := handle.Close(); err != nil {
		c.mu.Lock()
		c.closed = true
		c.poisoned = true
		c.mu.Unlock()
		return err
	}
	return nil
}

// Shutdown closes every admitted database before releasing the legacy lock.
// It refuses while calls are active, so a caller can retry after they finish.
func (c *ActiveCoordinator) Shutdown() error {
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
	if c.poisoned {
		c.mu.Unlock()
		return ErrActiveCleanup
	}
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	for _, state := range c.slots {
		if state.active > 0 {
			c.mu.Unlock()
			return novelruntime.ErrBusy
		}
	}
	c.closed = true
	handles := make([]Admitted, 0, len(c.slots))
	for _, state := range c.slots {
		if state.handle != nil {
			handles = append(handles, state.handle)
		}
	}
	c.mu.Unlock()
	var joined error
	for _, handle := range handles {
		joined = errors.Join(joined, handle.Close())
	}
	if joined != nil {
		c.mu.Lock()
		c.poisoned = true
		c.mu.Unlock()
		return errors.Join(ErrActiveCleanup, joined)
	}
	if err := c.lock.Close(); err != nil {
		c.mu.Lock()
		c.poisoned = true
		c.mu.Unlock()
		return errors.Join(ErrActiveCleanup, err)
	}
	c.mu.Lock()
	clear(c.slots)
	c.mu.Unlock()
	return nil
}
