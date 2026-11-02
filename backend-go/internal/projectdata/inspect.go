// Package projectdata composes filesystem capabilities with SQLite admission.
// Inspection never opens the source database with SQLite, grants write access,
// migrates schemas or connects handlers to author data.
package projectdata

import (
 "context"
 "errors"
 "os"
 "path/filepath"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

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
 directory,err:=os.MkdirTemp(scratchDirectory,"novel-go-inspect-");if err!=nil{return admission.Evidence{},err};defer os.RemoveAll(directory)
 if err:=os.Chmod(directory,0700);err!=nil{return admission.Evidence{},err}
 destination,err:=os.OpenRoot(directory);if err!=nil{return admission.Evidence{},err};defer destination.Close()
 proof,err:=project.StageDatabase(ctx,destination,maxBytes);if err!=nil{return admission.Evidence{},err}
 evidence,err:=admission.VerifySnapshot(ctx,filepath.Join(directory,"snapshot.db"),contract);if err!=nil{return admission.Evidence{},err}
 if err:=proof.VerifyUnchanged(ctx);err!=nil{return admission.Evidence{},err}
 if err:=ctx.Err();err!=nil{return admission.Evidence{},err}
 return evidence,nil
}
