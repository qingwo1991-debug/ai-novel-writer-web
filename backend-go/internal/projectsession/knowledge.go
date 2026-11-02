package projectsession

import (
 "context"
 "path/filepath"
 "strings"
 "sync"
 "unicode/utf8"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
 novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)

// KnowledgeManager owns immutable copied-state read captures only. It is separate
// from the editable scratch Manager and has no global/current database, source
// handle, durable author ownership or project:open RPC registration. Its leases
// protect an entire read call; they are not writer leases on an author project.
// Construct one per server with NewKnowledgeManager, not one per HTTP request.
// OpenKnowledge accepts only administrator configuration, never renderer paths.
// Same-owner refresh returns the SAME capture and lease without source I/O: it is
// explicitly not a request to recapture current author state or adopt new config.
// Release then open again is required to inspect source/config changes.
// Close and Release perform no filesystem cleanup: InspectKnowledge has already
// completed all proof/cleanup/unlock before a capture can be retained here.
 type KnowledgeManager struct {
 mu sync.Mutex
 leases *novelruntime.Leases
 mounts map[string]*mountedKnowledge
 closed bool
}
type mountedKnowledge struct {
 lease novelruntime.Lease
 capture *projectaccess.KnowledgeRead
 view *knowledgequery.View
 finalized map[string]int64
}
func NewKnowledgeManager()*KnowledgeManager{return &KnowledgeManager{leases:novelruntime.NewLeases(),mounts:map[string]*mountedKnowledge{}}}
func knowledgePath(path string)bool{return filepath.IsAbs(path)&&filepath.Clean(path)==path&&path!=string(filepath.Separator)&&!strings.ContainsAny(path,"\x00\\")&&utf8.ValidString(path)}
func knowledgeInputs(ctx context.Context,owner Owner,path string)error{if ctx==nil||!owner.valid()||!knowledgePath(path){return ErrContext};return ctx.Err()}

// OpenKnowledge captures a real native-admitted, fully cleaned private snapshot.
// Inspection runs while the manager mutex is held; no competing opener can adopt
// or replace this manager's same-path capture during admission. Source writer
// exclusion/proofs and every temporary file remain InspectKnowledge's duties.
func(m *KnowledgeManager)OpenKnowledge(ctx context.Context,owner Owner,config projectaccess.Config)(Context,error){
 if err:=knowledgeInputs(ctx,owner,config.ProjectPath);err!=nil{return Context{},err}
 if config.Knowledge==nil||!config.Knowledge.Native{return Context{},projectaccess.ErrConfiguration}
 m.mu.Lock();defer m.mu.Unlock()
 if m.closed{return Context{},ErrClosed};if err:=ctx.Err();err!=nil{return Context{},err}
 if mounted:=m.mounts[config.ProjectPath];mounted!=nil{if mounted.lease.Owner!=owner.key(){return Context{},novelruntime.ErrOwned};return fromLease(mounted.lease),nil}
 capture,report,err:=projectaccess.InspectKnowledge(ctx,config);if err!=nil{return Context{},err}
 if capture==nil||capture.View()==nil{return Context{},ErrContext}
 if err:=ctx.Err();err!=nil{return Context{},err}
 adopted,err:=m.adoptKnowledgeLocked(ctx,owner,config.ProjectPath,report.ProjectID,capture.View(),capture.FinalizedChapters());if err!=nil{return Context{},err}
 m.mounts[config.ProjectPath].capture=capture
 return adopted,nil
}

// adoptKnowledge is an unexported in-package fixture seam. Production callers
// cannot inject inspectors or fabricated views; OpenKnowledge uses the real
// projectaccess.InspectKnowledge and adopts only its completed capture.
func(m *KnowledgeManager)adoptKnowledge(ctx context.Context,owner Owner,path,id string,view *knowledgequery.View,finalized map[string]int64)(Context,error){
 if err:=knowledgeInputs(ctx,owner,path);err!=nil{return Context{},err}
 m.mu.Lock();defer m.mu.Unlock();return m.adoptKnowledgeLocked(ctx,owner,path,id,view,finalized)
}
func(m *KnowledgeManager)adoptKnowledgeLocked(ctx context.Context,owner Owner,path,id string,view *knowledgequery.View,finalized map[string]int64)(Context,error){
 if m.closed{return Context{},ErrClosed};if err:=ctx.Err();err!=nil{return Context{},err}
 if id==""||!utf8.ValidString(id)||len(id)>4096||view==nil{return Context{},ErrContext}
 if mounted:=m.mounts[path];mounted!=nil{if mounted.lease.Owner!=owner.key(){return Context{},novelruntime.ErrOwned};if mounted.lease.ProjectID!=id{return Context{},novelruntime.ErrNotOwned};return fromLease(mounted.lease),nil}
 // The public capture has these same fixed bounds. Repeat them here so neither
 // this private seam nor later reuse can retain an unbounded mutable input map.
 if len(finalized)>10000{return Context{},ErrContext};bytesLeft:=32*1024*1024
 copied:=make(map[string]int64,len(finalized));for key,value:=range finalized{if err:=ctx.Err();err!=nil{return Context{},err};if key==""||len(key)>bytesLeft||!utf8.ValidString(key)||value>9007199254740991||value< -9007199254740991{return Context{},ErrContext};bytesLeft-=len(key);copied[key]=value}
 lease,fresh,err:=m.leases.Acquire(path,id,owner.key());if err!=nil{return Context{},err}
 committed:=false;defer func(){if !committed{_ = m.leases.RollbackAcquire(lease,fresh)}}()
 if err:=ctx.Err();err!=nil{return Context{},err};if err:=m.leases.CommitAcquire(lease);err!=nil{return Context{},err}
 m.mounts[path]=&mountedKnowledge{lease:lease,view:view,finalized:copied};committed=true
 return fromLease(lease),nil
}

// BeginKnowledge lends one immutable capture with an exact cookie-session/tab,
// project path, project identity and lease fence held until done. Finalized state
// is copied per borrow, so response preparation cannot mutate subsequent calls.
// No source filesystem access occurs, including for an unknown/stale context.
func(m *KnowledgeManager)BeginKnowledge(ctx context.Context,owner Owner,expected Context)(*knowledgequery.View,map[string]int64,func(),error){
 if err:=knowledgeInputs(ctx,owner,expected.ProjectPath);err!=nil{return nil,nil,nil,err}
 m.mu.Lock();defer m.mu.Unlock();if m.closed{return nil,nil,nil,ErrClosed};if err:=ctx.Err();err!=nil{return nil,nil,nil,err}
 mounted:=m.mounts[expected.ProjectPath];if mounted==nil{return nil,nil,nil,novelruntime.ErrNotOwned}
 done,err:=m.leases.Begin(expected.lease(owner));if err!=nil{return nil,nil,nil,err}
 finalized:=make(map[string]int64,len(mounted.finalized));for id,chapter:=range mounted.finalized{if err:=ctx.Err();err!=nil{done();return nil,nil,nil,err};finalized[id]=chapter}
 if err:=ctx.Err();err!=nil{done();return nil,nil,nil,err};return mounted.view,finalized,done,nil
}
func(m *KnowledgeManager)Release(owner Owner,expected Context)error{
 if !owner.valid(){return ErrContext};m.mu.Lock();defer m.mu.Unlock();if m.closed{return ErrClosed}
 if m.mounts[expected.ProjectPath]==nil{return novelruntime.ErrNotOwned};if err:=m.leases.Release(expected.lease(owner));err!=nil{return err};delete(m.mounts,expected.ProjectPath);return nil
}
// Close refuses atomically while ANY capture is borrowed, preserving every mount
// and the open manager on ErrBusy. Call again after all whole-call holds finish.
func(m *KnowledgeManager)Close()error{m.mu.Lock();defer m.mu.Unlock();if m.closed{return nil};if err:=m.leases.ReleaseAll();err!=nil{return err};clear(m.mounts);m.closed=true;return nil}
