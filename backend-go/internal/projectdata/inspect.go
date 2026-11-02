// Package projectdata composes filesystem capabilities with SQLite admission.
// Inspection never opens the source database with SQLite, grants write access,
// migrates schemas or connects handlers to author data.
package projectdata

import (
 "bytes"
 "context"
 "errors"
 "os"
 "path/filepath"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

var ErrSourceChanged=errors.New("PROJECT_MIGRATION_SOURCE_CHANGED")
var ErrSnapshotInvalid=errors.New("PROJECT_SNAPSHOT_INVALID")

// Inspect requires caller-owned exclusion of all source writers throughout this
// function. Repeated sampling detects drift, but cannot replace writer exclusion
// or prove a concurrently changing multi-file SQLite set was consistent.
// scratchDirectory is an administrator-controlled scratch parent, never a path
// supplied by an RPC client. Only private temporary copies are passed to SQLite.
// maxBytes bounds the full physical file set; oversized sources fail, never truncate.
func Inspect(ctx context.Context,project *projectfs.Project,contract admission.SchemaContract,scratchDirectory string,maxBytes int64)(admission.Evidence,error){
 if err:=ctx.Err();err!=nil{return admission.Evidence{},err}
 if project==nil||maxBytes<=0{return admission.Evidence{},ErrSnapshotInvalid}
 snapshot,err:=project.SnapshotDatabase(maxBytes);if err!=nil{return admission.Evidence{},err}
 if _,ok:=snapshot.Files[""];!ok{return admission.Evidence{},ErrSnapshotInvalid}
 directory,err:=os.MkdirTemp(scratchDirectory,"novel-go-inspect-");if err!=nil{return admission.Evidence{},err};defer os.RemoveAll(directory)
 if err:=os.Chmod(directory,0700);err!=nil{return admission.Evidence{},err}
 var total int64
 for suffix,body:=range snapshot.Files{
  if suffix!=""&&suffix!="-wal"&&suffix!="-shm"&&suffix!="-journal"{return admission.Evidence{},ErrSnapshotInvalid}
  if int64(len(body))>maxBytes-total{return admission.Evidence{},ErrSnapshotInvalid};total+=int64(len(body))
  if err:=ctx.Err();err!=nil{return admission.Evidence{},err}
  file,err:=os.OpenFile(filepath.Join(directory,"snapshot.db"+suffix),os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600);if err!=nil{return admission.Evidence{},err}
  _,writeErr:=file.Write(body);closeErr:=file.Close();if err:=errors.Join(writeErr,closeErr);err!=nil{return admission.Evidence{},err}
 }
 evidence,err:=admission.VerifySnapshot(ctx,filepath.Join(directory,"snapshot.db"),contract);if err!=nil{return admission.Evidence{},err}
 after,err:=project.SnapshotDatabase(maxBytes);if err!=nil{return admission.Evidence{},err}
 if len(after.Files)!=len(snapshot.Files){return admission.Evidence{},ErrSourceChanged}
 for suffix,before:=range snapshot.Files{current,ok:=after.Files[suffix];if !ok||!bytes.Equal(before,current){return admission.Evidence{},ErrSourceChanged}}
 if err:=ctx.Err();err!=nil{return admission.Evidence{},err}
 return evidence,nil
}
