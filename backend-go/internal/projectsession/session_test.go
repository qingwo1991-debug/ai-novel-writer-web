package projectsession

import (
 "bytes"
 "context"
 "database/sql"
 "errors"
 "os"
 "path/filepath"
 "sync"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
 novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
func sessionFixture(t *testing.T)(*projectfs.Project,admission.SchemaContract,string){
 t.Helper();oracle:=os.Getenv("GO_SQLITE_CONTRACT");if oracle==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("oracle required")};t.Skip("GitHub original TypeScript schema oracle")}
 file,err:=os.Open(oracle);if err!=nil{t.Fatal(err)};contract,err:=admission.ParseContract(file);file.Close();if err!=nil{t.Fatal(err)}
 libraryPath:=t.TempDir();root:=filepath.Join(libraryPath,"synthetic-session-book");storage:=filepath.Join(root,".ai-novel");if err:=os.MkdirAll(storage,0700);err!=nil{t.Fatal(err)}
 manifest:=`{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`
 if err:=os.WriteFile(filepath.Join(storage,"project.json"),[]byte(manifest),0600);err!=nil{t.Fatal(err)}
 data,err:=os.ReadFile(filepath.Join(filepath.Dir(oracle),"baseline-v7.db"));if err!=nil{t.Fatal(err)};path:=filepath.Join(storage,"project.db");if err:=os.WriteFile(path,data,0600);err!=nil{t.Fatal(err)}
 dsn,err:=store.ExistingDSN(path);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)}
 if _,err:=db.Exec(`INSERT INTO drafts(id,chapter_number,version,content_id,word_count) VALUES(1,1,1,1,8)`);err!=nil{db.Close();t.Fatal(err)};if err:=db.Close();err!=nil{t.Fatal(err)}
 library,err:=projectfs.OpenLibrary(libraryPath);if err!=nil{t.Fatal(err)};t.Cleanup(func(){library.Close()});project,err:=library.OpenProject(root);if err!=nil{t.Fatal(err)};t.Cleanup(func(){project.Close()})
 return project,contract,path
}
func TestScratchSessionCASRefreshAndSourceNeverWritten(t *testing.T){
 project,contract,path:=sessionFixture(t);before,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
 manager:=New();t.Cleanup(func(){if err:=manager.Close();err!=nil{t.Error(err)}});scratch:=t.TempDir();owner:=Owner{SessionID: "cookie-session", ClientID: "browser-tab"};ctx:=context.Background()
 lease,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);if err!=nil{t.Fatal(err)}
 drafts,done,err:=manager.Begin(ctx,owner,lease);if err!=nil{t.Fatal(err)}
 full,err:=drafts.GetFull(ctx,1);if err!=nil||full==nil{t.Fatal(full,err)};baseline:=full.Content
 if err:=drafts.UpdateContentIfCurrent(ctx,1,"private session changed body 中文",12,&baseline);err!=nil{t.Fatal(err)}
 if err:=manager.Release(owner,lease);!errors.Is(err,novelruntime.ErrBusy){t.Fatal("release during repository operation",err)}
 if err:=manager.Close();!errors.Is(err,novelruntime.ErrBusy){t.Fatal("shutdown during repository operation",err)}
 done();done()
 same,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);if err!=nil||same!=lease{t.Fatal("refresh rotated",same,err)}
 drafts,done,err=manager.Begin(ctx,owner,same);if err!=nil{t.Fatal(err)};full,err=drafts.GetFull(ctx,1);done();if err!=nil||full.Content!="private session changed body 中文"{t.Fatal("refresh lost private edit",full,err)}
 after,err:=os.ReadFile(path);if err!=nil||!bytes.Equal(before,after){t.Fatal("source changed",err)}
 if err:=manager.Release(owner,lease);err!=nil{t.Fatal(err)}
 entries,err:=os.ReadDir(scratch);if err!=nil||len(entries)!=0{t.Fatal("scratch leaked",entries,err)}
 next,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);if err!=nil||next.LeaseID==lease.LeaseID{t.Fatal("lease reused",next,err)}
 if _,_,err:=manager.Begin(ctx,owner,lease);!errors.Is(err,novelruntime.ErrNotOwned){t.Fatal("stale context",err)}
}
func TestScratchSessionFrozenContextAndSingleOwner(t *testing.T){
 project,contract,_:=sessionFixture(t);manager:=New();defer manager.Close();scratch:=t.TempDir();owner:=Owner{SessionID: "session-a", ClientID: "tab-a"};ctx:=context.Background()
 lease,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);if err!=nil{t.Fatal(err)}
 for _,other:=range []Owner{{SessionID: "session-a", ClientID: "tab-b"}, {SessionID: "session-b", ClientID: "tab-a"}, {SessionID: "", ClientID: "tab-a"}}{
  if _,err:=manager.OpenScratchCopy(ctx,other,project,contract,scratch,32*1024*1024);err==nil{t.Fatal("ownership stolen",other)}
  if _,done,err:=manager.Begin(ctx,other,lease);err==nil{done();t.Fatal("context stolen",other)}
 }
 for _,changed:=range []Context{{lease.ProjectID,"wrong",lease.ProjectPath},{"wrong",lease.LeaseID,lease.ProjectPath},{lease.ProjectID,lease.LeaseID,filepath.Join(lease.ProjectPath,"..")}}{
  if _,done,err:=manager.Begin(ctx,owner,changed);err==nil{done();t.Fatal("stale accepted",changed)}
 }
 var wg sync.WaitGroup
 for range 12{wg.Go(func(){same,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);if err!=nil||same!=lease{t.Errorf("concurrent refresh: %+v %v",same,err)}})};wg.Wait()
 entries,err:=os.ReadDir(scratch);if err!=nil||len(entries)!=1{t.Fatal("duplicate scratch mount",len(entries),err)}
}
func TestScratchFailedAdmissionRollsBackAndContextParsing(t *testing.T){
 project,contract,_:=sessionFixture(t);manager:=New();scratch:=t.TempDir();ctx:=context.Background();owner:=Owner{SessionID: "s", ClientID: "t"}
 if _,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,1);err==nil{t.Fatal("small budget accepted")}
 entries,err:=os.ReadDir(scratch);if err!=nil||len(entries)!=0{t.Fatal("failed open leaked",entries,err)}
 other:=Owner{SessionID: "other-session", ClientID: "other-tab"};if _,err:=manager.OpenScratchCopy(ctx,other,project,contract,scratch,32*1024*1024);err!=nil{t.Fatal("failed opener retained lease",err)}
 if err:=manager.Close();err!=nil{t.Fatal(err)};if err:=manager.Close();err!=nil{t.Fatal(err)}
 if _,err:=manager.OpenScratchCopy(ctx,owner,project,contract,scratch,32*1024*1024);!errors.Is(err,ErrClosed){t.Fatal(err)}
 for _,value:=range []any{nil,"/path",map[string]any{},map[string]any{"projectId":"p","leaseId":"l","projectPath":"/x","extra":true}}{if _,err:=ParseContext(value);!errors.Is(err,ErrContext){t.Fatal(value,err)}}
 parsed,err:=ParseContext(map[string]any{"projectId":"p","leaseId":"l","projectPath":"/x"});if err!=nil||parsed.ProjectPath!="/x"{t.Fatal(parsed,err)}
}
