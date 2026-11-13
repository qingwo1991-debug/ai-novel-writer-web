// Package runtime contains browser-session concurrency primitives. It never
// launches Node processes and does not authorize a project based on a path alone.
package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

var (
	ErrOwned    = errors.New("PROJECT_OPEN_IN_ANOTHER_TAB")
	ErrNotOwned = errors.New("PROJECT_SESSION_NOT_OWNED")
	ErrBusy     = errors.New("PROJECT_REQUEST_IN_FLIGHT")
)

type Lease struct {
	ProjectPath string `json:"projectPath"`
	ProjectID   string `json:"projectId"`
	LeaseID     string `json:"leaseId"`
	Owner       string `json:"-"`
}
type leaseState struct {
	lease     Lease
	active    int
	committed bool
}

// Leases is per-server, never package global. Owner must include both session
// identity and browser tab identity, not merely the upstream login username.
type Leases struct {
	mu       sync.Mutex
	projects map[string]*leaseState
}

func NewLeases() *Leases { return &Leases{projects: make(map[string]*leaseState)} }
func token() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// Acquire must only be called with a validated canonical physical project root
// and manifest identity. A refresh by the existing owner retains its lease.
// Every successful open/persistence must CommitAcquire before reporting success,
// including a same-owner refresh. Open failures must RollbackAcquire with the
// returned fresh flag. A failed commit means the acquisition was revoked while
// opening: the caller must clean up and must not report a successful open.
func (m *Leases) Acquire(root, projectID, owner string) (lease Lease, fresh bool, err error) {
	if root == "" || projectID == "" || owner == "" {
		return Lease{}, false, ErrNotOwned
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.projects[root]; state != nil {
		if state.lease.Owner != owner {
			return Lease{}, false, ErrOwned
		}
		if state.lease.ProjectID != projectID {
			return Lease{}, false, ErrNotOwned
		}
		return state.lease, false, nil
	}
	id, err := token()
	if err != nil {
		return Lease{}, false, err
	}
	lease = Lease{ProjectPath: root, ProjectID: projectID, Owner: owner, LeaseID: id}
	m.projects[root] = &leaseState{lease: lease}
	return lease, true, nil
}
func sameLease(a, b Lease) bool {
	return a.ProjectPath == b.ProjectPath && a.ProjectID == b.ProjectID && a.LeaseID == b.LeaseID && a.Owner == b.Owner
}

// Begin fences the complete operation, not just its initial HTTP validation.
// The returned release function is safe to call multiple times.
func (m *Leases) Begin(expected Lease) (func(), error) {
	m.mu.Lock()
	state := m.projects[expected.ProjectPath]
	if state == nil || !sameLease(state.lease, expected) {
		m.mu.Unlock()
		return nil, ErrNotOwned
	}
	state.active++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.projects[expected.ProjectPath] == state {
				state.active--
			}
		})
	}, nil
}

// Release refuses to expire/replace a session while an operation is in flight.
func (m *Leases) Release(expected Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.projects[expected.ProjectPath]
	if state == nil || !sameLease(state.lease, expected) {
		return ErrNotOwned
	}
	if state.active > 0 {
		return ErrBusy
	}
	delete(m.projects, expected.ProjectPath)
	return nil
}

// ReleaseAll atomically clears this lease set only when all operations are idle.
// The owner of the complete Leases instance may use it for orderly shutdown;
// this is never a browser-callable force release or ownership transfer.
func (m *Leases) ReleaseAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, state := range m.projects {
		if state.active > 0 {
			return ErrBusy
		}
	}
	clear(m.projects)
	return nil
}

// CommitAcquire adopts the exact acquisition after a successful open. Adoption
// is idempotent and prevents a concurrent failed opener from rolling it back.
// It does not bypass the active-operation fence used by explicit Release.
func (m *Leases) CommitAcquire(expected Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.projects[expected.ProjectPath]
	if state == nil || !sameLease(state.lease, expected) {
		return ErrNotOwned
	}
	state.committed = true
	return nil
}

// RollbackAcquire only removes a fresh, uncommitted acquisition. A successful
// same-owner opener may already have committed the shared lease; its ownership
// must survive another opener's failure. The check and deletion are atomic.
func (m *Leases) RollbackAcquire(expected Lease, fresh bool) error {
	if !fresh {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.projects[expected.ProjectPath]
	if state == nil || !sameLease(state.lease, expected) {
		return ErrNotOwned
	}
	if state.committed {
		return nil
	}
	if state.active > 0 {
		return ErrBusy
	}
	delete(m.projects, expected.ProjectPath)
	return nil
}
