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
	handle    Admitted
	key       string
	latest    uint64
	active    int
	idle      chan struct{}
	switching bool
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
		c.mu.Lock()
		stale := c.closed || state.latest != request
		c.mu.Unlock()
		if stale {
			return Context{}, ErrStaleOpen
		}
		return current.Context(), nil
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
			_ = candidate.Close()
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
	adopted = true
	c.mu.Unlock()
	if previous != nil {
		if err := previous.Close(); err != nil {
			c.mu.Lock()
			state.handle = nil
			state.key = ""
			state.switching = false
			c.mu.Unlock()
			return Context{}, errors.Join(err, candidate.Close())
		}
	}
	c.mu.Lock()
	state.switching = false
	c.mu.Unlock()
	return identity, nil
}

// Begin fences a complete repository operation using the exact frozen context.
// A switching or closing slot cannot issue new operations. done is idempotent.
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
	if state == nil || state.handle == nil || !sameContext(state.handle.Context(), expected) {
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

// CloseProject first fences new calls, then waits for all exact-context calls
// to finish. Cancellation restores the old handle and its ability to serve.
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
	if state == nil || state.handle == nil || !sameContext(state.handle.Context(), expected) {
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
	return handle.Close()
}

// Shutdown closes every admitted database before releasing the legacy lock.
// It refuses while calls are active, so a caller can retry after they finish.
func (c *ActiveCoordinator) Shutdown() error {
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
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
	clear(c.slots)
	c.mu.Unlock()
	var joined error
	for _, handle := range handles {
		joined = errors.Join(joined, handle.Close())
	}
	return errors.Join(joined, c.lock.Close())
}
