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
 ErrOwned = errors.New("PROJECT_OPEN_IN_ANOTHER_TAB")
 ErrNotOwned = errors.New("PROJECT_SESSION_NOT_OWNED")
 ErrBusy = errors.New("PROJECT_REQUEST_IN_FLIGHT")
)

type Lease struct {
 ProjectPath string `json:"projectPath"`
 ProjectID string `json:"projectId"`
 LeaseID string `json:"leaseId"`
 Owner string `json:"-"`
}
type leaseState struct { lease Lease; active int }

// Leases is per-server, never package global. Owner must include both session
// identity and browser tab identity, not merely the upstream login username.
type Leases struct { mu sync.Mutex; projects map[string]*leaseState }
func NewLeases() *Leases { return &Leases{projects:make(map[string]*leaseState)} }
func token() (string,error) { var bytes [32]byte; if _,err:=rand.Read(bytes[:]); err!=nil { return "",err }; return hex.EncodeToString(bytes[:]),nil }

// Acquire must only be called with a validated canonical physical project root
// and manifest identity. A refresh by the existing owner retains its lease.
// Open/persistence failures must RollbackAcquire only if fresh=true.
func (m *Leases) Acquire(root,projectID,owner string) (lease Lease,fresh bool,err error) {
 if root=="" || projectID=="" || owner=="" { return Lease{},false,ErrNotOwned }
 m.mu.Lock(); defer m.mu.Unlock()
 if state:=m.projects[root]; state!=nil {
  if state.lease.Owner!=owner { return Lease{},false,ErrOwned }
  if state.lease.ProjectID!=projectID { return Lease{},false,ErrNotOwned }
  return state.lease,false,nil
 }
 id,err:=token(); if err!=nil { return Lease{},false,err }
 lease=Lease{ProjectPath:root,ProjectID:projectID,Owner:owner,LeaseID:id}
 m.projects[root]=&leaseState{lease:lease}; return lease,true,nil
}
func sameLease(a,b Lease) bool { return a.ProjectPath==b.ProjectPath && a.ProjectID==b.ProjectID && a.LeaseID==b.LeaseID && a.Owner==b.Owner }
// Begin fences the complete operation, not just its initial HTTP validation.
// The returned release function is safe to call multiple times.
func (m *Leases) Begin(expected Lease) (func(),error) {
 m.mu.Lock(); state:=m.projects[expected.ProjectPath]
 if state==nil || !sameLease(state.lease,expected) { m.mu.Unlock(); return nil,ErrNotOwned }
 state.active++; m.mu.Unlock()
 var once sync.Once
 return func(){ once.Do(func(){m.mu.Lock(); defer m.mu.Unlock(); if m.projects[expected.ProjectPath]==state { state.active-- }}) },nil
}
// Release refuses to expire/replace a session while an operation is in flight.
func (m *Leases) Release(expected Lease) error {
 m.mu.Lock(); defer m.mu.Unlock(); state:=m.projects[expected.ProjectPath]
 if state==nil || !sameLease(state.lease,expected) { return ErrNotOwned }
 if state.active>0 { return ErrBusy }
 delete(m.projects,expected.ProjectPath); return nil
}
func (m *Leases) RollbackAcquire(lease Lease,fresh bool) error { if !fresh { return nil }; return m.Release(lease) }
