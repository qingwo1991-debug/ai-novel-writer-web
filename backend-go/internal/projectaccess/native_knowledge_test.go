//go:build lance_native && cgo

package projectaccess

import (
 "bytes"
 "context"
 "crypto/sha256"
 "errors"
 "encoding/json"
 "io/fs"
 "os"
 "path/filepath"
 "reflect"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func nativeAccessFixture(t *testing.T)Config{
 t.Helper();contract:=os.Getenv("GO_LANCE_CONTRACTROOT");if contract==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("native copied fixture required")};t.Skip("GitHub synthetic Lance fixture required")}
 c:=accessFixture(t);storage:=filepath.Join(c.ProjectPath,".ai-novel")
 if err:=os.CopyFS(storage,os.DirFS(filepath.Join(contract,"copy")));err!=nil{t.Fatal(err)}
 c.Knowledge=&KnowledgeConfig{Copy:projectfs.KnowledgeBudget{MaxBytes:128*1024*1024,MaxEntries:10000,MaxDepth:32},Metadata:knowledgecopy.Budget{MaxMetadataBytes:8*1024*1024,MaxCopies:100},Native:true}
 return c
}
func nativeSourceManifest(t *testing.T,c Config)map[string][32]byte{
 t.Helper();out:=map[string][32]byte{};root:=filepath.Join(c.ProjectPath,".ai-novel")
 if err:=filepath.WalkDir(root,func(path string,entry fs.DirEntry,err error)error{if err!=nil{return err};relative,err:=filepath.Rel(root,path);if err!=nil{return err};if entry.IsDir(){out[relative+"/"]=[32]byte{};return nil};data,err:=os.ReadFile(path);if err!=nil{return err};out[relative]=sha256.Sum256(data);return nil});err!=nil{t.Fatal(err)};return out
}
func TestNativeKnowledgePreflightSQLiteAndLanceDomain(t *testing.T){
 c:=nativeAccessFixture(t);before:=nativeSourceManifest(t,c)
 report,err:=Preflight(context.Background(),c);if err!=nil{t.Fatal(err)}
 if report.Knowledge==nil||report.Knowledge.Native==nil{t.Fatal("native evidence missing")}
 native:=report.Knowledge.Native;if native.TableCount!=6||native.Documents!=3||native.Chunks!=4||native.Spaces!=4||native.EmbeddingRows!=7||native.ActiveGeneration==nil||*native.ActiveGeneration!=1{t.Fatal("incomplete native evidence",native)}
 if report.SQLite.UserVersion!=7||!report.LegacyGatewayExcluded||report.Knowledge.Metadata.Copies!=3{t.Fatal("missing joint copied evidence")}
 links:=report.Knowledge.CopyLinks;if links==nil||links.Documents!=3||links.Managed!=3||links.Present!=3||links.Current!=3||links.Missing!=0||links.Unreferenced!=0{t.Fatal("copy reference evidence missing",links)}
 if !reflect.DeepEqual(before,nativeSourceManifest(t,c)){t.Fatal("original asset bytes or membership changed")};accessClean(t,c)
}
func TestNativeKnowledgeAbsentLanceNeverCreated(t *testing.T){
 c:=accessFixture(t);c.Knowledge=&KnowledgeConfig{Copy:projectfs.KnowledgeBudget{MaxBytes:1024,MaxEntries:100,MaxDepth:8},Metadata:knowledgecopy.Budget{MaxMetadataBytes:1024,MaxCopies:10},Native:true}
 report,err:=Preflight(context.Background(),c);if err!=nil||report.Knowledge==nil||report.Knowledge.Native==nil||report.Knowledge.Native.TableCount!=0{t.Fatal(report,err)}
 if _,err:=os.Lstat(filepath.Join(c.ProjectPath,".ai-novel/lancedb"));!errors.Is(err,fs.ErrNotExist){t.Fatal("created missing author Lance",err)};accessClean(t,c)
}
func TestNativeKnowledgeFailureZeroReportAndCleanup(t *testing.T){
 for _,kind:=range []string{"invalid-native","sqlite-invalid","portable-before","portable-after","source-after","legacy-after","budget","cancel-after","native-domain","rename-scratch"}{t.Run(kind,func(t *testing.T){
  c:=nativeAccessFixture(t);storage:=filepath.Join(c.ProjectPath,".ai-novel");ctx,cancel:=context.WithCancel(context.Background());defer cancel()
  switch kind{case "invalid-native":file:=filepath.Join(storage,"lancedb/chunks.lance");if err:=os.Rename(file,file+"-hidden");err!=nil{t.Fatal(err)};case "sqlite-invalid":writeAccess(t,filepath.Join(storage,"project.db"),[]byte("invalid synthetic SQLite"));case "portable-before":writeAccess(t,filepath.Join(storage,"portable-runtime-freeze.json"),[]byte("not authority"));case "budget":c.Knowledge.Copy.MaxBytes=1;case "native-domain":file:=filepath.Join(storage,"embedding-spaces.json");raw,err:=os.ReadFile(file);if err!=nil{t.Fatal(err)};var registry map[string]any;if err:=json.Unmarshal(raw,&registry);err!=nil{t.Fatal(err)};registry["activeGeneration"]=2;changed,err:=json.Marshal(registry);if err!=nil{t.Fatal(err)};writeAccess(t,file,changed)}
  hook:=func(*projectfs.Project)error{switch kind{case "portable-after":return os.WriteFile(filepath.Join(storage,"portable-runtime-freeze.json"),[]byte("late"),0600);case "source-after":return os.WriteFile(filepath.Join(storage,"knowledge-copies","late.json"),[]byte("new"),0600);case "legacy-after":return os.WriteFile(filepath.Join(storage,"vectors.json"),[]byte("late"),0600);case "cancel-after":cancel();case "rename-scratch":entries,err:=os.ReadDir(c.ScratchParent);if err!=nil||len(entries)!=1{return errors.New("expected one private scratch")};old:=filepath.Join(c.ScratchParent,entries[0].Name());moved:=old+"-moved";if err:=os.Rename(old,moved);err!=nil{return err};if err:=os.Mkdir(old,0700);err!=nil{return err};return os.WriteFile(filepath.Join(old,"replacement"),[]byte("preserve"),0600)};return nil}
  report,err:=preflight(ctx,c,hook);if err==nil||!reflect.DeepEqual(report,Report{}){t.Fatal("refusal did not return zero report",report,err)};if kind=="cancel-after"&&!errors.Is(err,context.Canceled){t.Fatal(err)};if kind=="rename-scratch"{entries,err:=os.ReadDir(c.ScratchParent);if err!=nil||len(entries)!=2{t.Fatal(entries,err)};for _,entry:=range entries{path:=filepath.Join(c.ScratchParent,entry.Name());children,err:=os.ReadDir(path);if err!=nil{t.Fatal(err)};if len(children)==0{continue};if len(children)!=1||children[0].Name()!="replacement"{t.Fatal("owned native or database scratch leaked",children)};data,readErr:=os.ReadFile(filepath.Join(path,"replacement"));if readErr!=nil||string(data)!="preserve"{t.Fatal("replacement changed",readErr)}};guard,lockErr:=writerlock.AcquireLegacy(c.DataRoot);if lockErr!=nil{t.Fatal("writer lock leaked",lockErr)};if err:=guard.Close();err!=nil{t.Fatal(err)};return};accessClean(t,c)
 })}
}
func TestNativeKnowledgeLegacyLockHeldThroughNativeAndSQLite(t *testing.T){
 c:=nativeAccessFixture(t);reached:=false
 _,err:=preflight(context.Background(),c,func(*projectfs.Project)error{reached=true;if other,err:=writerlock.AcquireLegacy(c.DataRoot);err==nil{other.Close();return errors.New("legacy writer admitted inside native preflight")};return nil})
 if err!=nil||!reached{t.Fatal("native stage did not remain under exclusion",err)};accessClean(t,c)
}
func nativeOutputFixture(t *testing.T)(*os.Root,string,*nativeOutput){t.Helper();path:=t.TempDir();if err:=os.Chmod(path,0700);err!=nil{t.Fatal(err)};parent,err:=os.OpenRoot(path);if err!=nil{t.Fatal(err)};t.Cleanup(func(){parent.Close()});output,err:=newNativeOutput(parent);if err!=nil{t.Fatal(err)};return parent,path,output}
func TestNativeKnowledgeOutputCleanupFixedOnly(t *testing.T){
 for _,kind:=range []string{"normal","unknown","symlink","rename"}{t.Run(kind,func(t *testing.T){
  parent,path,output:=nativeOutputFixture(t);known:=filepath.Join(path,nativeOutputName,"chunks.arrows");writeAccess(t,known,[]byte("attempt-owned partial output"));writeAccess(t,filepath.Join(path,nativeOutputName,"domain.json"),[]byte(`{"tables":[]}`))
  switch kind{case "unknown":writeAccess(t,filepath.Join(path,nativeOutputName,"unknown"),[]byte("preserve"));case "symlink":if err:=os.Remove(known);err!=nil{t.Fatal(err)};if err:=os.Symlink("not-followed",known);err!=nil{t.Fatal(err)};case "rename":if err:=parent.Rename(nativeOutputName,nativeOutputName+"-moved");err!=nil{t.Fatal(err)};if err:=parent.Mkdir(nativeOutputName,0700);err!=nil{t.Fatal(err)};writeAccess(t,filepath.Join(path,nativeOutputName,"replacement"),[]byte("preserve"))}
  err:=output.cleanup();if kind=="normal"{if err!=nil{t.Fatal(err)};entries,err:=os.ReadDir(path);if err!=nil||len(entries)!=0{t.Fatal("normal output leaked",entries,err)};return}
  if !errors.Is(err,ErrCleanup){t.Fatal("unsafe output not reported",err)}
  switch kind{case "unknown":data,err:=os.ReadFile(filepath.Join(path,nativeOutputName,"unknown"));if err!=nil||!bytes.Equal(data,[]byte("preserve")){t.Fatal("unknown removed")};if _,err:=os.Lstat(known);!errors.Is(err,fs.ErrNotExist){t.Fatal("owned known file left",err)};case "symlink":info,err:=os.Lstat(known);if err!=nil||info.Mode()&os.ModeSymlink==0{t.Fatal("replacement link deleted",err)};case "rename":data,err:=os.ReadFile(filepath.Join(path,nativeOutputName,"replacement"));if err!=nil||!bytes.Equal(data,[]byte("preserve")){t.Fatal("replacement removed")};entries,err:=os.ReadDir(filepath.Join(path,nativeOutputName+"-moved"));if err!=nil||len(entries)!=0{t.Fatal("renamed output known files not cleaned",entries,err)}}
 })}
}
