package importsource

import (
 "archive/zip"
 "bytes"
 "encoding/json"
 "io"
 "strings"
 "sync"
 "testing"
 "time"
)
func source(name,text string) CapturedSource { return CapturedSource{DisplayName:name,Reader:strings.NewReader(text),Identity:Identity{CanonicalLocation:"captured:"+name,FileIdentity:"stage:"+name}} }
func newTestStore(t *testing.T,options StoreOptions) *Store { t.Helper(); s,err:=NewStore(bytes.Repeat([]byte{107},32),options); if err!=nil { t.Fatal(err) }; return s }
func TestDraftUnitsExactWhitespace(t *testing.T) {
 for _,tc:=range []struct{text string; want int}{{"x\ufeffy",2},{"x\u0085y",3},{"x\u200by",3},{"𠀀𰀀",2},{"中文 café e\u0301 123 🙂",7},{"a’中b",3},{"\u0345",1}} { if got:=CountDraftUnits(tc.text); got!=tc.want { t.Fatalf("%q: %d != %d",tc.text,got,tc.want) } }
}
func TestInspectionBoundOneUseAndIsolation(t *testing.T) {
 now:=time.Unix(10,0); store:=newTestStore(t,StoreOptions{Now:func() time.Time{return now}}); binding:=Binding{"caller","generation",Reference}
 summary,err:=store.Inspect(binding,[]CapturedSource{source("novel.txt","第一章 开始\n中文\n第二章 后来\nHello world")}); if err!=nil { t.Fatal(err) }
 if summary.ChapterCount!=2 || summary.TotalWords!=4 { t.Fatalf("%+v",summary) }
 serialized,_:=json.Marshal(summary); for _,secret:=range []string{"captured:","stage:","Alias","中文"} { if strings.Contains(string(serialized),secret) { t.Fatalf("leaked %s",secret) } }
 summary.Preview[0].Title="mutated"
 peek,err:=store.Peek(summary.InspectionID,binding); if err!=nil || peek.Preview[0].Title!="开始" { t.Fatalf("snapshot mutation: %+v %v",peek,err) }
 for _,wrong:=range []Binding{{"other","generation",Reference},{"caller","other",Reference},{"caller","generation",AuthorManuscript}} { if _,err=store.Consume(summary.InspectionID,summary.PreviewCAS,wrong); err==nil { t.Fatal("foreign receipt accepted") } }
 if _,err=store.Consume(summary.InspectionID,strings.Repeat("0",64),binding); err==nil { t.Fatal("CAS accepted") }
 inspection,err:=store.Consume(summary.InspectionID,summary.PreviewCAS,binding); if err!=nil || len(inspection.Chapters)!=2 { t.Fatal(err) }
 if _,err=store.Consume(summary.InspectionID,summary.PreviewCAS,binding); err==nil { t.Fatal("replayed") }
 summary,err=store.Inspect(binding,[]CapturedSource{source("novel.md","body")}); if err!=nil { t.Fatal(err) }; now=now.Add(10*time.Minute)
 if _,err=store.Peek(summary.InspectionID,binding); err==nil { t.Fatal("expired receipt accepted") }
}
func TestReplacementAndFailuresRevoke(t *testing.T) {
 store:=newTestStore(t,StoreOptions{}); b:=Binding{"c","g",Reference}; first,err:=store.Inspect(b,[]CapturedSource{source("n.txt","body")}); if err!=nil { t.Fatal(err) }
 second,err:=store.Inspect(b,[]CapturedSource{source("n.txt","different")}); if err!=nil { t.Fatal(err) }; if first.PreviewCAS==second.PreviewCAS { t.Fatal("CAS unchanged") }
 if _,err=store.Peek(first.InspectionID,b); err==nil { t.Fatal("replacement not revoked") }
 if _,err=store.Inspect(b,[]CapturedSource{source("../bad.txt","body")}); err==nil { t.Fatal("path accepted") }
 if _,err=store.Peek(second.InspectionID,b); err==nil { t.Fatal("failed reselection retained old") }
}
func TestBoundedReadCountAndSourceNumbers(t *testing.T) {
 small:=DefaultLimits(); small.MaxTotalBytes=4
 if _,err:=Parse("n.txt",strings.NewReader("12345"),small); err==nil || err.Error()!="IMPORT_SOURCE_BYTES_EXCEEDED" { t.Fatal(err) }
 if _,err:=Parse("n.txt",strings.NewReader("第一章 标题"),DefaultLimits()); err==nil || err.Error()!="IMPORT_SOURCE_TITLE_ONLY" { t.Fatal(err) }
 store:=newTestStore(t,StoreOptions{}); b:=Binding{"c","g",Reference}; summary,err:=store.Inspect(b,[]CapturedSource{source("n.md","第十章 同\na\n第十章 同\nb\nChapter 0: Zero\nc")}); if err!=nil { t.Fatal(err) }
 result,err:=store.Consume(summary.InspectionID,summary.PreviewCAS,b); if err!=nil { t.Fatal(err) }
 for i,want:=range []int{10,1,3} { if result.Chapters[i].SourceChapterNumber!=want || result.Chapters[i].Number!=i+1 { t.Fatalf("%+v",result.Chapters) } }
}
func TestConcurrentConsumeExactlyOnce(t *testing.T) {
 store:=newTestStore(t,StoreOptions{}); b:=Binding{"c","g",Reference}; summary,err:=store.Inspect(b,[]CapturedSource{source("n.txt","body")}); if err!=nil { t.Fatal(err) }
 var wg sync.WaitGroup; results:=make(chan bool,20)
 for i:=0;i<20;i++ { wg.Add(1); go func(){defer wg.Done(); _,err:=store.Consume(summary.InspectionID,summary.PreviewCAS,b); results<-err==nil}() }; wg.Wait(); close(results)
 count:=0; for ok:=range results { if ok { count++ } }; if count!=1 { t.Fatal(count) }
}
type revokeReader struct { store *Store; caller string; done bool }
func (r *revokeReader) Read(p []byte) (int,error) { if r.done { return 0,io.EOF }; r.done=true; r.store.RevokeCaller(r.caller); return copy(p,"body"),nil }
func TestRevocationDuringCapturedReadCannotResurrect(t *testing.T) {
 store:=newTestStore(t,StoreOptions{}); b:=Binding{"c","g",Reference}; captured:=source("n.txt","unused"); captured.Reader=&revokeReader{store:store,caller:b.Caller}
 if _,err:=store.Inspect(b,[]CapturedSource{captured}); err==nil || err.Error()!="IMPORT_INSPECTION_EXPIRED" { t.Fatal(err) }; if store.ActiveCount()!=0 { t.Fatal("resurrected") }
}
func zipFixture(t *testing.T,files map[string]string) []byte {
 t.Helper(); var b bytes.Buffer; w:=zip.NewWriter(&b)
 for name,body:=range files { f,err:=w.Create(name); if err!=nil { t.Fatal(err) }; if _,err=f.Write([]byte(body)); err!=nil { t.Fatal(err) } }; if err:=w.Close(); err!=nil { t.Fatal(err) }; return b.Bytes()
}
func TestMaliciousEPUB(t *testing.T) {
 for name,files:=range map[string]map[string]string{"traversal":{"../bad":"x"},"absolute":{"/bad":"x"},"backslash":{"a\\b":"x"},"drive":{"C:/bad":"x"},"duplicate-normalized":{"a/../b":"x","b":"y"},"no-container":{"book.opf":"x"}} {
  t.Run(name,func(t *testing.T){_,err:=EPUBPreview(zipFixture(t,files)); if err==nil { t.Fatal("accepted") }})
 }
}
func TestSourceEncodingAndIdentityValidation(t *testing.T) {
 if _,err:=Parse("n.txt",bytes.NewReader([]byte{255}),DefaultLimits()); err==nil { t.Fatal("invalid UTF-8") }
 if _,err:=NewStore([]byte("renderer key"),StoreOptions{}); err==nil { t.Fatal("short key") }
 a,err:=EncodeIdentities([]Identity{{"  captured:cafe\u0301.txt\ufeff"," stage:1 "}},Reference,bytes.Repeat([]byte{107},32),false); if err!=nil { t.Fatal(err) }
 b,err:=EncodeIdentities([]Identity{{"captured:café.txt","stage:1"}},AuthorManuscript,bytes.Repeat([]byte{107},32),false); if err!=nil { t.Fatal(err) }
 if a[0]!=b[0] { t.Fatal("NFC/trim/purpose alias mismatch") }
 _,err=EncodeIdentities([]Identity{{"captured:\x00.txt",""}},Reference,bytes.Repeat([]byte{107},32),false); if err==nil { t.Fatal("NUL accepted") }
}
