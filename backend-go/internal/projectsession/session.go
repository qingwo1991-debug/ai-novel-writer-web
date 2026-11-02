// Package projectsession composes source admission, private-copy repositories and
// per-session/tab leases. It is an isolated migration lane: changes are made ONLY
// to a disposable scratch copy and are NEVER committed back to an author project.
// It is intentionally not exposed as project:open until coordinated SQLite,
// LanceDB, global settings and durable author writes are implemented.
package projectsession

import (
 "context"
 "database/sql"
 "errors"
 "os"
 "path/filepath"
 "sync"
 "strings"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
 novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

var ErrClosed=errors.New("PROJECT_SESSION_MANAGER_CLOSED")
var ErrContext=errors.New("PROJECT_SESSION_CONTEXT_REQUIRED")

// Owner contains both cookie session and browser tab identity. The upstream login
// username is not a browser session and cannot be used alone as an owner.
type Owner struct {SessionID string; ClientID string}
func(o Owner) key()string{return o.SessionID+":"+o.ClientID}
func(o Owner) valid()bool{return o.SessionID!=""&&o.ClientID!=""&&!strings.ContainsAny(o.SessionID+o.ClientID,":\x00")}

type Context struct {
 ProjectID string `json:"projectId"`
 LeaseID string `json:"leaseId"`
 ProjectPath string `json:"projectPath"`
}
func ParseContext(value any)(Context,error){
 object,ok:=value.(map[string]any);if !ok||len(object)!=3{return Context{},ErrContext}
 id,okID:=object["projectId"].(string);lease,okLease:=object["leaseId"].(string);path,okPath:=object["projectPath"].(string)
 if !okID||!okLease||!okPath||id==""||lease==""||path==""{return Context{},ErrContext}
 return Context{ProjectID:id,LeaseID:lease,ProjectPath:path},nil
}
func(l Context) lease(o Owner)novelruntime.Lease{return novelruntime.Lease{ProjectID:l.ProjectID,LeaseID:l.LeaseID,ProjectPath:l.ProjectPath,Owner:o.key()}}
type mounted struct {lease novelruntime.Lease;db *sql.DB;directory string}
type Manager struct {mu sync.Mutex;leases *novelruntime.Leases;mounts map[string]*mounted;closed bool}
func New()*Manager{return &Manager{leases:novelruntime.NewLeases(),mounts:make(map[string]*mounted)}}

// OpenScratchCopy requires caller exclusion of source writers/checkpointers for
// its entire duration, plus a trusted private scratch parent. No source SQLite
// file is ever opened. project must stay open throughout this call, but need not
// outlive a successful copy. Same-owner refresh retains the SAME scratch DB and
// lease, never overwrites edits by copying the source again. This is NOT durable
// author-project opening or migration publication.
func(m *Manager)OpenScratchCopy(ctx context.Context,owner Owner,project *projectfs.Project,contract admission.SchemaContract,scratchParent string,maxBytes int64)(Context,error){
 if ctx==nil||!owner.valid()||project==nil{return Context{},ErrContext}
 if err:=ctx.Err();err!=nil{return Context{},err}
 m.mu.Lock();defer m.mu.Unlock()
 if m.closed{return Context{},ErrClosed}
 root,id:=project.Path(),project.ProjectID()
 if mounted:=m.mounts[root];mounted!=nil{
  if mounted.lease.Owner!=owner.key(){return Context{},novelruntime.ErrOwned}
  if mounted.lease.ProjectID!=id{return Context{},novelruntime.ErrNotOwned}
  // Validate the supplied capability still names the same physical project.
  // Never read source SQLite merely to refresh an already-mounted scratch copy.
  manifest,err:=project.OpenFileRead(projectfs.ManifestName);if err!=nil{return Context{},err};if err:=manifest.Close();err!=nil{return Context{},err}
  return fromLease(mounted.lease),nil
 }
 lease,fresh,err:=m.leases.Acquire(root,id,owner.key());if err!=nil{return Context{},err}
 committed:=false;defer func(){if !committed{_ = m.leases.RollbackAcquire(lease,fresh)}}()
 directory,err:=os.MkdirTemp(scratchParent,"novel-go-session-");if err!=nil{return Context{},err}
 keep:=false;defer func(){if !keep{_ = os.RemoveAll(directory)}}()
 if err:=os.Chmod(directory,0700);err!=nil{return Context{},err}
 destination,err:=os.OpenRoot(directory);if err!=nil{return Context{},err};defer destination.Close()
 proof,err:=project.StageDatabase(ctx,destination,maxBytes);if err!=nil{return Context{},err}
 path:=filepath.Join(directory,"snapshot.db")
 if _,err:=admission.VerifySnapshot(ctx,path,contract);err!=nil{return Context{},err}
 if err:=proof.VerifyUnchanged(ctx);err!=nil{return Context{},err}
 dsn,err:=store.ExistingDSN(path);if err!=nil{return Context{},err}
 db,err:=sql.Open("sqlite3",dsn);if err!=nil{return Context{},err}
 db.SetMaxOpenConns(4)
 owned:=false;defer func(){if !owned{_ = db.Close()}}()
 if err:=db.PingContext(ctx);err!=nil{return Context{},err}
 if err:=ctx.Err();err!=nil{return Context{},err}
 if err:=m.leases.CommitAcquire(lease);err!=nil{return Context{},err}
 m.mounts[root]=&mounted{lease:lease,db:db,directory:directory}
 committed=true;owned=true;keep=true
 return fromLease(lease),nil
}
func fromLease(lease novelruntime.Lease)Context{return Context{ProjectID:lease.ProjectID,LeaseID:lease.LeaseID,ProjectPath:lease.ProjectPath}}

// Begin holds the exact session/tab lease through the whole repository call. A
// release/manager close refuses while this hold exists. Returned cleanup is
// idempotent. An unknown path never causes a filesystem open or DB creation.
func(m *Manager)Begin(ctx context.Context,owner Owner,expected Context)(*store.Drafts,func(),error){
 if ctx==nil||!owner.valid(){return nil,nil,ErrContext};if err:=ctx.Err();err!=nil{return nil,nil,err}
 m.mu.Lock();defer m.mu.Unlock();if m.closed{return nil,nil,ErrClosed}
 mounted:=m.mounts[expected.ProjectPath];if mounted==nil{return nil,nil,novelruntime.ErrNotOwned}
 done,err:=m.leases.Begin(expected.lease(owner));if err!=nil{return nil,nil,err}
 return store.NewDrafts(mounted.db),done,nil
}
func(m *Manager)Release(owner Owner,expected Context)error{
 m.mu.Lock();defer m.mu.Unlock();if m.closed{return ErrClosed};if !owner.valid(){return ErrContext}
 mounted:=m.mounts[expected.ProjectPath];if mounted==nil{return novelruntime.ErrNotOwned}
 if err:=m.leases.Release(expected.lease(owner));err!=nil{return err}
 delete(m.mounts,expected.ProjectPath)
 return errors.Join(mounted.db.Close(),os.RemoveAll(mounted.directory))
}
// Close refuses atomically if ANY operation is active. It cannot revoke or close
// a DB underneath an in-flight repository operation. Retry only after holds end.
func(m *Manager)Close()error{
 m.mu.Lock();defer m.mu.Unlock();if m.closed{return nil}
 // Probe all active states without destructive releases. Acquiring an additional
 // hold cannot reveal activity, so use the lease manager's non-destructive gate.
 if err:=m.leases.ReleaseAll();err!=nil{return err}
 m.closed=true;var joined error
 for root,mounted:=range m.mounts{joined=errors.Join(joined,mounted.db.Close(),os.RemoveAll(mounted.directory));delete(m.mounts,root)}
 return joined
}
