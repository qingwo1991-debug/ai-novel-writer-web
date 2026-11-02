package archiveformat

import (
 "bytes"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "io"
 "os"
 "path/filepath"
 "reflect"
 "strings"
 "testing"
)

type observation struct { OK bool `json:"ok"`; Value json.RawMessage `json:"value"`; Code string `json:"code"` }
type oracleDocument struct {
 Paths []struct {Value string; Original observation}
 Manifests []struct {Name,JSON string; Original observation}
 Archives []struct {Name,File string; Limits json.RawMessage; Original observation; NativeReject bool}
 Freezes []struct {Name,JSON string; Original observation}
 Authorities []struct {Name,JSON string; Original observation}
 Builds []struct {Name string; Manifest Manifest; Sources []struct {Entry Entry; File string}; File string; Receipt Receipt}
 NativeHardening []json.RawMessage
}
func oracle(t *testing.T)(oracleDocument,string){t.Helper();file:=os.Getenv("GO_ARCHIVE_FORMAT_CONTRACT");if file==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("GO_ARCHIVE_FORMAT_CONTRACT required on GitHub")};t.Skip("original exported JS differential fixtures are generated on GitHub only")};root:=os.Getenv("RUNNER_TEMP");if root==""||file!=filepath.Join(root,"novel-go-archive-format","contract.json"){t.Fatal("contract must be disposable runner fixture")};data,e:=os.ReadFile(file);if e!=nil{t.Fatal(e)};var out oracleDocument;if e=json.Unmarshal(data,&out);e!=nil{t.Fatal(e)};return out,filepath.Dir(file)}
func compareOutcome(t *testing.T,original observation,err error){t.Helper();if original.OK!=(err==nil){t.Fatalf("original ok=%v code=%s native=%v",original.OK,original.Code,err)};if err!=nil&&Code(err)!=original.Code{t.Fatalf("original code=%s native=%s",original.Code,Code(err))}}
func sameJSON(t *testing.T,a any,b []byte){t.Helper();encoded,e:=json.Marshal(a);if e!=nil{t.Fatal(e)};var av,bv any;if e=json.Unmarshal(encoded,&av);e!=nil{t.Fatal(e)};if e=json.Unmarshal(b,&bv);e!=nil{t.Fatal(e)};if !reflect.DeepEqual(av,bv){t.Fatalf("JSON mismatch native=%s original=%s",encoded,b)}}
func TestOriginalExportedFormatDifferential(t *testing.T){
 fixtures,root:=oracle(t)
 if len(fixtures.Paths)<20||len(fixtures.Manifests)<15||len(fixtures.Archives)<35||len(fixtures.NativeHardening)<3{t.Fatal("missing required differential coverage")}
 for _,f:=range fixtures.Paths{t.Run("path/"+f.Value,func(t *testing.T){key,e:=PathKey(f.Value,1024);compareOutcome(t,f.Original,e);if e==nil{sameJSON(t,key,f.Original.Value)}})}
 for _,f:=range fixtures.Manifests{t.Run("manifest/"+f.Name,func(t *testing.T){m,e:=ParseManifest([]byte(f.JSON),Limits{});compareOutcome(t,f.Original,e);if e==nil{sameJSON(t,m,f.Original.Value)}})}
 for _,f:=range fixtures.Archives{t.Run("zip/"+f.Name,func(t *testing.T){data,e:=os.ReadFile(filepath.Join(root,f.File));if e!=nil{t.Fatal(e)};l,e:=ParseLimits(f.Limits);if e!=nil{t.Fatal(e)};a,e:=Inspect(bytes.NewReader(data),int64(len(data)),l);if f.NativeReject{if e==nil{t.Fatal("native hardening case accepted")};t.Logf("separate original observation: ok=%v code=%s; native=%v",f.Original.OK,f.Original.Code,e);return};compareOutcome(t,f.Original,e);if e!=nil{return};var expected struct{Manifest Manifest;Receipt Receipt};if e=json.Unmarshal(f.Original.Value,&expected);e!=nil{t.Fatal(e)};if !reflect.DeepEqual(a.Manifest(),expected.Manifest)||a.Receipt()!=expected.Receipt{t.Fatal("manifest/receipt differs from exported original")};stage:=&memoryStage{};if e=a.Stage(stage);e!=nil{t.Fatal(e)};if !stage.complete||stage.aborted{t.Fatal("successful stage not sealed")};for _,entry:=range a.Manifest().Entries{sum:=sha256.Sum256(stage.files[entry.Path].Bytes());if hex.EncodeToString(sum[:])!=entry.SHA256{t.Fatal("staged payload mismatch")}}})}
 for _,f:=range fixtures.Freezes{t.Run("freeze/"+f.Name,func(t *testing.T){g,e:=ParseRuntimeFreeze([]byte(f.JSON));compareOutcome(t,f.Original,e);if e==nil{sameJSON(t,map[string]bool{"active":g.Active(),"frozen":g.IsFrozen("drafts","1"),"mutable":g.AssertMutable("drafts","1")==nil},f.Original.Value)}})}
 for _,f:=range fixtures.Authorities{t.Run("authority/"+f.Name,func(t *testing.T){a,e:=ParseTransferAuthority([]byte(f.JSON));compareOutcome(t,f.Original,e);if e==nil{sameJSON(t,a,f.Original.Value)}})}
 t.Run("portable-boundary",func(t *testing.T){var frozen,transferred []byte;for _,f:=range fixtures.Freezes{if f.Name=="valid"{frozen=[]byte(f.JSON)}};for _,f:=range fixtures.Authorities{if f.Name=="valid"{transferred=[]byte(f.JSON)}};a,e:=ParseTransferAuthority(transferred);if e!=nil{t.Fatal(e)};m,_:=exampleManifest();m.OriginProjectID=a.OriginProjectID;m.SnapshotGeneration=a.SnapshotGeneration;m.Entries=append(m.Entries,Entry{Path:".ai-novel/project.db",SHA256:a.PortableDatabaseSHA256,Disposition:"portable-database"});g,authority,e:=ValidatePortableBoundary(m,frozen,transferred);if e!=nil||!g.IsFrozen("drafts","1")||authority==nil{t.Fatal("valid freeze/portable facts",e)};if _,_,e=ValidatePortableBoundary(m,nil,transferred);Code(e)!="PORTABLE_RUNTIME_FREEZE_INVALID"{t.Fatal("authority without guard accepted")};m.SnapshotGeneration="other";if _,_,e=ValidatePortableBoundary(m,frozen,transferred);e==nil{t.Fatal("cross-generation freeze accepted")}})
 for _,f:=range fixtures.Builds{t.Run("build/"+f.Name,func(t *testing.T){sources:=[]Source{};for _,s:=range f.Sources{data,e:=os.ReadFile(filepath.Join(root,s.File));if e!=nil{t.Fatal(e)};sources=append(sources,Source{s.Entry,bytes.NewReader(data)})};var output bytes.Buffer;receipt,e:=Build(&output,f.Manifest,sources,Limits{});if e!=nil{t.Fatal(e)};expected,e:=os.ReadFile(filepath.Join(root,f.File));if e!=nil{t.Fatal(e)};if !bytes.Equal(output.Bytes(),expected)||receipt!=f.Receipt{t.Fatal("native streamed ZIP bytes/receipt differ from real original builder")}})}
}

type memoryStage struct{files map[string]*bytes.Buffer;complete,aborted bool;failOpen,failWrite,failClose,failComplete,failAbort bool;closed int}
type stageWriter struct{stage *memoryStage;buffer *bytes.Buffer}
func(w *stageWriter)Write(b []byte)(int,error){if w.stage.failWrite{return 0,errors.New("/private/path must not escape")};return w.buffer.Write(b)}
func(w *stageWriter)Close()error{w.stage.closed++;if w.stage.failClose{return errors.New("/private/close")};return nil}
func(s *memoryStage)Open(e Entry)(io.WriteCloser,error){if s.failOpen{return nil,errors.New("/private/open")};if s.files==nil{s.files=map[string]*bytes.Buffer{}};if s.files[e.Path]!=nil{return nil,errors.New("exists")};b:=&bytes.Buffer{};s.files[e.Path]=b;return &stageWriter{s,b},nil}
func(s *memoryStage)Complete(Manifest,Receipt)error{if s.failComplete{return errors.New("/private/complete")};s.complete=true;return nil}
func(s *memoryStage)Abort()error{s.aborted=true;if s.failAbort{return errors.New("/private/abort")};s.files=nil;return nil}
func exampleManifest() (Manifest,[]Source){data:=[]byte("synthetic prose");sum:=sha256.Sum256(data);entry:=Entry{"正文/a.txt",uint64(len(data)),hex.EncodeToString(sum[:]),"author-content"};return Manifest{1,7,"11111111-1111-4111-8111-111111111111","g1","2026-09-20T00:00:00.000Z",uint64(len(data)),uint64(len(data)),[]Entry{entry},map[string]uint64{"chapters":1},[]OmittedItem{},[]string{},[]string{}},[]Source{{entry,bytes.NewReader(data)}}}
func sampleArchive(t *testing.T)(*Archive,[]byte){t.Helper();m,s:=exampleManifest();var output bytes.Buffer;if _,e:=Build(&output,m,s,Limits{});e!=nil{t.Fatal(e)};data:=append([]byte(nil),output.Bytes()...);a,e:=Inspect(bytes.NewReader(data),int64(len(data)),Limits{});if e!=nil{t.Fatal(e)};return a,data}
func TestStageFailureAndCleanupAreNeverPublication(t *testing.T){
 for _,name:=range []string{"open","write","close","complete","abort"}{t.Run(name,func(t *testing.T){a,_:=sampleArchive(t);s:=&memoryStage{};switch name{case "open":s.failOpen=true;case "write":s.failWrite=true;case "close":s.failClose=true;case "complete":s.failComplete=true;case "abort":s.failWrite=true;s.failClose=true;s.failAbort=true};e:=a.Stage(s);if e==nil||!s.aborted||s.complete{t.Fatal("failure must abort and never complete")};if strings.Contains(e.Error(),"/private/"){t.Fatal("arbitrary stage error leaked filesystem data")};if name!="open"&&s.closed!=1{t.Fatal("opened writer not closed")};if name=="abort"&&(!strings.Contains(e.Error(),"ARCHIVE_STAGE_CLEANUP_FAILED")||!strings.Contains(e.Error(),"ARCHIVE_STAGE_CLOSE_FAILED")){t.Fatal("cleanup or close failure lost")};if name!="abort"&&len(s.files)!=0{t.Fatal("failed staging payload retained")}})}
}
func TestBuildRejectsIncompleteSourcesAndShortWrites(t *testing.T){
 m,s:=exampleManifest();s[0].Reader=strings.NewReader("bad");var out bytes.Buffer;r,e:=Build(&out,m,s,Limits{});if Code(e)!="PORTABLE_ARCHIVE_SOURCE_CHANGED"||r!=(Receipt{}){t.Fatal("incomplete build must not return receipt")};if len(out.Bytes())<4{t.Fatal("test must reach partial streamed output")};if _,e=Inspect(bytes.NewReader(out.Bytes()),int64(out.Len()),Limits{});e==nil{t.Fatal("incomplete output accepted")};m,s=exampleManifest();if r,e=Build(shortWriter{},m,s,Limits{});e==nil||r!=(Receipt{}){t.Fatal("short write accepted")};m,s=exampleManifest();s[0].Reader=strings.NewReader("synthetic prose trailing");if _,e=Build(io.Discard,m,s,Limits{});Code(e)!="PORTABLE_ARCHIVE_SOURCE_CHANGED"{t.Fatal("source extra bytes accepted")}
}
type shortWriter struct{}
func(shortWriter)Write(b []byte)(int,error){if len(b)==0{return 0,nil};return len(b)-1,nil}
func TestStrictCentralLocalMetadata(t *testing.T){
 a,data:=sampleArchive(t);if a.Receipt().EntryCount!=1{t.Fatal("receipt")};rec:=a.records[1]
 for _,mutate:=range []func([]byte){func(b []byte){b[rec.offset+30]^=1},func(b []byte){b[rec.offset+6]^=1},func(b []byte){b[rec.end-12]^=1},func(b []byte){b[rec.data]^=1}}{copy:=append([]byte(nil),data...);mutate(copy);if _,e:=Inspect(bytes.NewReader(copy),int64(len(copy)),Limits{});e==nil{t.Fatal("malicious ZIP metadata/content accepted")}}
 if _,e:=Inspect(bytes.NewReader(append(data,0)),int64(len(data)+1),Limits{});e==nil{t.Fatal("trailing bytes accepted")}
}
func TestSourceMutationBetweenInspectAndStage(t *testing.T){
 a,data:=sampleArchive(t);data[a.records[1].data]^=1;s:=&memoryStage{};if e:=a.Stage(s);e==nil||!s.aborted||s.complete||len(s.files)!=0{t.Fatal("mutated reader published staged content")}
}
func TestJSONDuplicateAndSurrogateHardening(t *testing.T){
 m,_:=exampleManifest();data,e:=m.Bytes(Limits{});if e!=nil{t.Fatal(e)}
 duplicate:=bytes.Replace(data,[]byte(`"formatVersion":1`),[]byte(`"formatVersion":9,"formatVersion":1`),1);if _,e=ParseManifest(duplicate,Limits{});e==nil{t.Fatal("duplicate JSON key accepted")}
 lone:=bytes.Replace(data,[]byte(`正文/a.txt`),[]byte(`\ud800.txt`),1);if _,e=ParseManifest(lone,Limits{});e==nil{t.Fatal("lone surrogate path repaired and admitted")}
 for _,raw:=range []string{`{"maxEntries":0}`,`{"maxEntries":-1}`,`{"maxEntries":1.5}`,`{"maxEntries":9007199254740992}`}{if _,e:=ParseLimits([]byte(raw));Code(e)!="PORTABLE_ARCHIVE_LIMIT_INVALID"{t.Fatal("unsafe limit",raw)}}
}
func TestZIP64AndManifestBounds(t *testing.T){
 for _,rec:=range []record{{name:"big.bin",size:uint32Max,offset:0,flags:0x808},{name:"far.bin",size:1,offset:uint32Max,flags:0x808}}{central:=centralHeader(rec);if le.Uint16(central[6:])!=45||le.Uint16(central[30:])==0{t.Fatal("ZIP64 central metadata missing")}}
 local:=localHeader("big.bin",uint32Max,0,true);if le.Uint16(local[4:])!=45||le.Uint32(local[18:])!=0xffffffff||le.Uint64(local[41:])!=uint32Max{t.Fatal("ZIP64 local header")}
 end:=endRecords(65535,12,13,false);if len(end)!=98||le.Uint64(end[32:])!=65535||le.Uint32(end[56:])!=0x07064b50{t.Fatal("ZIP64 count end")}
 m,_:=exampleManifest();m.DeclaredCompressedBytes=0;if _,e:=m.Bytes(Limits{});Code(e)!="PORTABLE_ARCHIVE_LIMIT_EXCEEDED"{t.Fatal("zero compressed bomb declaration")};for _,p:=range []string{"../x","Ａ/．．/x","x/CON.txt","x/😀.txt","x\\y"}{if _,e:=PathKey(p,1024);e==nil{t.Fatal("unsafe path",p)}};if _,e:=ParseManifest(bytes.Repeat([]byte{' '},MaxManifestBytes+1),Limits{});Code(e)!="PORTABLE_ARCHIVE_LIMIT_EXCEEDED"{t.Fatal("manifest not bounded")}
}
