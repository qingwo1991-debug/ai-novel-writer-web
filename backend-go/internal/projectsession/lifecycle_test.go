package projectsession

import (
 "context"
 "errors"
 "os"
 "path/filepath"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
 novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)
func TestScratchRefreshRejectsPhysicalSameUUIDReplacement(t *testing.T){
 project,contract,_:=sessionFixture(t);manager:=New();defer manager.Close();owner:=Owner{"session","tab"};scratch:=t.TempDir()
 if _,err:=manager.OpenScratchCopy(context.Background(),owner,project,contract,scratch,32*1024*1024);err!=nil{t.Fatal(err)}
 path:=project.Path();old:=path+"-moved";if err:=os.Rename(path,old);err!=nil{t.Fatal(err)}
 storage:=filepath.Join(path,".ai-novel");if err:=os.MkdirAll(storage,0700);err!=nil{t.Fatal(err)}
 for _,name:=range []string{"project.json","project.db"}{data,err:=os.ReadFile(filepath.Join(old,".ai-novel",name));if err!=nil{t.Fatal(err)};if err:=os.WriteFile(filepath.Join(storage,name),data,0600);err!=nil{t.Fatal(err)}}
 library,err:=projectfs.OpenLibrary(filepath.Dir(path));if err!=nil{t.Fatal(err)};defer library.Close();replacement,err:=library.OpenProject(path);if err!=nil{t.Fatal(err)};defer replacement.Close()
 if _,err:=manager.OpenScratchCopy(context.Background(),owner,replacement,contract,scratch,32*1024*1024);!errors.Is(err,projectfs.ErrChanged){t.Fatal("same UUID physical replacement accepted",err)}
}
func TestScratchCleanupFailureRetainsRetryUntilSuccess(t *testing.T){
 for _,action:=range []string{"release","close"}{t.Run(action,func(t *testing.T){
  project,contract,_:=sessionFixture(t);manager:=New();defer manager.Close();owner:=Owner{"s","t"};scratch:=t.TempDir()
  lease,err:=manager.OpenScratchCopy(context.Background(),owner,project,contract,scratch,32*1024*1024);if err!=nil{t.Fatal(err)}
  failure:=errors.New("synthetic cleanup denied");manager.removeAll=func(string)error{return failure}
  if action=="release"{err=manager.Release(owner,lease)}else{err=manager.Close()};if !errors.Is(err,failure){t.Fatal("cleanup error hidden",err)}
  if len(manager.pending)!=1{t.Fatal("cleanup retry record discarded")}
  if _,done,err:=manager.Begin(context.Background(),owner,lease);err==nil{done();t.Fatal("revoked context regained access")}else if action=="release"&&!errors.Is(err,novelruntime.ErrNotOwned){t.Fatal(err)}
  if err:=manager.Cleanup();!errors.Is(err,failure){t.Fatal("retry error hidden",err)}
  manager.removeAll=os.RemoveAll
  if err:=manager.Close();err!=nil{t.Fatal("retry close",err)}
  if len(manager.pending)!=0{t.Fatal("cleanup retained after success")};entries,err:=os.ReadDir(scratch);if err!=nil||len(entries)!=0{t.Fatal(entries,err)}
 })}
}
