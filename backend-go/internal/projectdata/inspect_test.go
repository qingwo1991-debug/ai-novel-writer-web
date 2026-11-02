package projectdata

import (
 "bytes"
 "context"
 "database/sql"
 "errors"
 "os"
 "path/filepath"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestInspectRejectsInvalidCapabilityAndCanceledContext(t *testing.T){
 if _,err:=Inspect(context.Background(),nil,nil,t.TempDir(),1024);!errors.Is(err,ErrSnapshotInvalid){t.Fatal(err)}
 ctx,cancel:=context.WithCancel(context.Background());cancel()
 if _,err:=Inspect(ctx,nil,nil,t.TempDir(),1024);!errors.Is(err,context.Canceled){t.Fatal(err)}
}

// The fixture and its exact DDL hashes are generated exclusively on GitHub from
// the existing TypeScript migration lane. No original project or host DB used.
func TestInspectPhysicalWALSetFromOriginalSchema(t *testing.T){
 source:=os.Getenv("GO_SQLITE_CONTRACT");if source==""{t.Skip("GitHub-only TypeScript schema oracle")}
 file,err:=os.Open(source);if err!=nil{t.Fatal(err)}
 contract,err:=admission.ParseContract(file);file.Close();if err!=nil{t.Fatal(err)}
 libraryPath:=t.TempDir();projectPath:=filepath.Join(libraryPath,"synthetic-book");storage:=filepath.Join(projectPath,".ai-novel")
 if err:=os.MkdirAll(storage,0700);err!=nil{t.Fatal(err)}
 manifest:=[]byte(`{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`)
 if err:=os.WriteFile(filepath.Join(storage,"project.json"),manifest,0600);err!=nil{t.Fatal(err)}
 body,err:=os.ReadFile(filepath.Join(filepath.Dir(source),"baseline-v7.db"));if err!=nil{t.Fatal(err)}
 databasePath:=filepath.Join(storage,"project.db");if err:=os.WriteFile(databasePath,body,0600);err!=nil{t.Fatal(err)}
 // Convert ONLY our synthetic temporary clone to WAL. Keep this connection open
 // to retain WAL/SHM files. It remains quiescent while Inspect samples the set.
 dsn,err:=store.ExistingDSN(databasePath);if err!=nil{t.Fatal(err)}
 db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer db.Close();db.SetMaxOpenConns(1)
 if _,err:=db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;");err!=nil{t.Fatal(err)}
 if _,err:=db.Exec("INSERT INTO contents(body) VALUES(?)","synthetic WAL-only body 中文 😀");err!=nil{t.Fatal(err)}
 if info,err:=os.Stat(databasePath+"-wal");err!=nil||info.Size()==0{t.Fatal("fixture must have uncheckpointed WAL",err)}
 library,err:=projectfs.OpenLibrary(libraryPath);if err!=nil{t.Fatal(err)};defer library.Close()
 project,err:=library.OpenProject(projectPath);if err!=nil{t.Fatal(err)};defer project.Close()
 before,err:=project.SnapshotDatabase(32*1024*1024);if err!=nil{t.Fatal(err)}
 scratch:=t.TempDir()
 evidence,err:=Inspect(context.Background(),project,contract,scratch,32*1024*1024);if err!=nil{t.Fatal(err)}
 if evidence.UserVersion!=7||evidence.TableCounts["contents"]!=2{t.Fatal("WAL-only row lost",evidence)}
 after,err:=project.SnapshotDatabase(32*1024*1024);if err!=nil{t.Fatal(err)}
 if len(before.Files)!=len(after.Files){t.Fatal("source sidecar membership changed")}
 for suffix,body:=range before.Files{if !bytes.Equal(body,after.Files[suffix]){t.Fatal("inspection wrote original physical file",suffix)}}
 entries,err:=os.ReadDir(scratch);if err!=nil||len(entries)!=0{t.Fatal("private snapshot leaked",err,len(entries))}
}
