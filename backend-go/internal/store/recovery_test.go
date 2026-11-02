package store

import (
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "errors"
 "strings"
 "sync"
 "testing"
)

func recoveryFixture(t *testing.T)(*sql.DB,*Recovery,string){
 t.Helper();db,_,path:=blueprintFixture(t)
 mustExec(t,db,"DELETE FROM recovery_candidates")
 if err:=NewBlueprints(db).Upsert(context.Background(),blueprintData(700001));err!=nil{t.Fatal(err)}
 return db,NewRecovery(db),path
}
func recoveryInput()map[string]any{
 return map[string]any{"runId":" \ufeffrun ","stepId":" step ","projectId":" project ","chapterNumber":700001,
  "chapterTitle":"  第七章😀  ","source":blueprintData(700001),"sourceDraft":nil,
  "visibleText":"\ufeff<think>秘密\n😀</think> 正文😀\u0085 ","failureCode":" failed ","failureReason":" why "}
}
func recoveryRecord(t *testing.T,r *Recovery,input map[string]any)map[string]any{
 t.Helper();out,err:=r.Record(context.Background(),input);if err!=nil{t.Fatal(err)};return out
}
func recoveryWant(t *testing.T,err,want error){t.Helper();if !errors.Is(err,want){t.Fatalf("error=%v want=%v",err,want)}}
func recoveryCount(t *testing.T,db *sql.DB)int{
 t.Helper();var n int;if err:=db.QueryRow("SELECT COUNT(*) FROM recovery_candidates").Scan(&n);err!=nil{t.Fatal(err)};return n
}
func TestRecoveryOrderedSourceHashAndVisibleUnicode(t *testing.T){
 db,r,_:=recoveryFixture(t);ctx:=context.Background();db.SetMaxOpenConns(1)
 input:=recoveryInput();out:=recoveryRecord(t,r,input)
 expected:=`{"chapterNumber":700001,"title":"第700001章 暗门😀","role":"转折","purpose":"目的`+"\u2028"+`<>&","keyEvents":"甲`+"\u2029"+`乙","characters":["阿青","😀"],"suspenseHook":"疑问？","userGuidance":""}`
 var serialized string;if err:=db.QueryRow("SELECT source_snapshot FROM recovery_candidates WHERE candidate_id=?",out["candidateId"]).Scan(&serialized);err!=nil{t.Fatal(err)}
 if serialized!=expected{t.Fatalf("ordered JSON\ngot %q\nwant %q",serialized,expected)}
 hash:=sha256.Sum256([]byte(expected));if out["sourceHash"]!=hex.EncodeToString(hash[:]){t.Fatalf("hash %v",out)}
 if out["visibleText"]!="正文😀\u0085"||out["sourceCurrent"]!=true||out["runId"]!="run"||out["stepId"]!="step"||out["projectId"]!="project"||out["chapterTitle"]!="第七章😀"||out["replacesCandidateId"]!=nil||out["resolvedAt"]!=nil{t.Fatalf("projection %v",out)}
 if len(out)!=16{t.Fatalf("unexpected output fields %v",out)}
 updated,err:=r.UpdatePending(ctx," \ufeff"+out["candidateId"].(string)+" ","secret</THINK> <think>gone</think> \ufeffedited😀 ")
 if err!=nil||updated["visibleText"]!="edited😀"{t.Fatalf("update %v %v",updated,err)}
 rows,err:=r.ListPending(ctx);if err!=nil||len(rows)!=1||rows[0]["contentHash"]!=recoveryHash("edited😀"){t.Fatalf("list %v %v",rows,err)}
 recoveryWant(t,r.Resolve(ctx,out["candidateId"].(string),"continued"),nil)
 rows,err=r.ListPending(ctx);if err!=nil||rows==nil||len(rows)!=0{t.Fatalf("empty %v %v",rows,err)}
 recoveryWant(t,r.Resolve(ctx,out["candidateId"].(string),"discarded"),ErrRecoveryNotPending)
}
func TestRecoveryValidationAndUnsupportedNativeInputs(t *testing.T){
 _,r,_:=recoveryFixture(t);ctx:=context.Background()
 for _,test:=range []struct{key string;value any;want error}{
  {"runId","\ufeff ",ErrRecoveryIdentityInvalid},{"stepId",strings.Repeat("😀",81),ErrRecoveryIdentityInvalid},
  {"chapterNumber",1.5,ErrRecoveryChapterInvalid},{"chapterNumber",float64(9007199254740992),ErrRecoveryChapterInvalid},
  {"sourceDraft",map[string]any{"id":1,"version":0},ErrRecoverySourceDraftInvalid},
  {"visibleText","<think>only hidden",ErrRecoveryTextEmpty},{"visibleText",string([]byte{255}),ErrRecoveryInputUnsupported},
  {"chapterTitle",true,ErrRecoveryInputUnsupported},{"failureCode",strings.Repeat("x",159)+"😀",ErrRecoveryInputUnsupported},
  {"failureReason",strings.Repeat("x",1999)+"😀",ErrRecoveryInputUnsupported},
 }{input:=recoveryInput();input[test.key]=test.value;out,err:=r.Record(ctx,input);if out!=nil{t.Fatalf("invalid wrote output %v",out)};recoveryWant(t,err,test.want)}
 for _,chars:=range []any{nil,"abc",[]any{"ok",3},[]any{string([]byte{255})},map[string]any{"name":"甲"}}{
  input:=recoveryInput();input["source"].(map[string]any)["characters"]=chars;_,err:=r.Record(ctx,input);recoveryWant(t,err,ErrRecoveryInputUnsupported)
 }
 input:=recoveryInput();input["source"].(map[string]any)["chapterNumber"]=1;_,err:=r.Record(ctx,input);recoveryWant(t,err,ErrRecoverySourceMismatch)
 input=recoveryInput();delete(input,"sourceDraft");_,err=r.Record(ctx,input);recoveryWant(t,err,ErrRecoverySourceDraftInvalid)
 input=recoveryInput();input["failureCode"]=strings.Repeat("😀",81);out:=recoveryRecord(t,r,input);if out["failureCode"]!=strings.Repeat("😀",80){t.Fatalf("UTF16 truncation %v",out)}
 for _,r:=range []*Recovery{nil,NewRecovery(nil)}{
  _,err:=r.Record(ctx,nil);recoveryWant(t,err,ErrRecoveryProjectNotOpen)
  rows,err:=r.ListPending(ctx);if rows!=nil{t.Fatal(rows)};recoveryWant(t,err,ErrRecoveryProjectNotOpen)
  _,err=r.UpdatePending(ctx,"","");recoveryWant(t,err,ErrRecoveryProjectNotOpen)
  recoveryWant(t,r.Resolve(ctx,"","bad"),ErrRecoveryActionInvalid)
  recoveryWant(t,r.Resolve(ctx,"","discarded"),ErrRecoveryProjectNotOpen)
 }
}
func TestRecoverySourceCurrentBlueprintDraftAndLegacy(t *testing.T){
 db,r,_:=recoveryFixture(t);ctx:=context.Background();id:=recoveryRecord(t,r,recoveryInput())["candidateId"].(string)
 mustExec(t,db,"UPDATE blueprints SET notes='not a source field',notes_updated_at='now' WHERE chapter_number=700001")
 rows,err:=r.ListPending(ctx);if err!=nil||rows[0]["sourceCurrent"]!=true{t.Fatalf("notes %v %v",rows,err)}
 mustExec(t,db,"UPDATE blueprints SET characters='[\"阿青\",\"other\"]' WHERE chapter_number=700001")
 _,err=r.UpdatePending(ctx,id," ");recoveryWant(t,err,ErrRecoverySourceChanged)
 recoveryWant(t,r.Resolve(ctx,id,"continued"),ErrRecoverySourceChanged)
 if err:=NewBlueprints(db).Upsert(ctx,blueprintData(700001));err!=nil{t.Fatal(err)}
 mustExec(t,db,"INSERT INTO contents(id,body) VALUES(700001,'body')")
 mustExec(t,db,"INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count) VALUES(700001,700001,1,'draft',700001,4)")
 rows,err=r.ListPending(ctx);if err!=nil||rows[0]["sourceCurrent"]!=false{t.Fatalf("null identity with latest %v %v",rows,err)}
 input:=recoveryInput();input["sourceDraft"]=map[string]any{"id":700001,"version":1};current:=recoveryRecord(t,r,input)["candidateId"].(string)
 mustExec(t,db,"UPDATE contents SET body='changed body' WHERE id=700001; UPDATE drafts SET status='archived' WHERE id=700001")
 if out,err:=r.UpdatePending(ctx,current,"body edit does not change draft identity");err!=nil||out["sourceCurrent"]!=true{t.Fatalf("metadata-only identity %v %v",out,err)}
 mustExec(t,db,"UPDATE recovery_candidates SET source_draft_identity_captured=0 WHERE candidate_id=?",current)
 _,err=r.UpdatePending(ctx,current,"edit");recoveryWant(t,err,ErrRecoverySourceChanged)
 mustExec(t,db,"UPDATE recovery_candidates SET source_draft_identity_captured=1,source_draft_version=NULL WHERE candidate_id=?",current)
 rows,err=r.ListPending(ctx);if err!=nil||rows[1]["sourceCurrent"]!=false{t.Fatalf("one null identity %v %v",rows,err)}
 mustExec(t,db,"UPDATE recovery_candidates SET source_draft_version=1 WHERE candidate_id=?",current)
 mustExec(t,db,"INSERT INTO contents(id,body) VALUES(700002,'new'); INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count) VALUES(700002,700001,2,'draft',700002,3)")
 recoveryWant(t,r.Resolve(ctx,current,"continued"),ErrRecoverySourceChanged)
 recoveryWant(t,r.Resolve(ctx,current,"discarded"),nil)
}
func TestRecoveryIntegrityAndResolveOriginalAsymmetry(t *testing.T){
 db,r,_:=recoveryFixture(t);ctx:=context.Background()
 first:=recoveryRecord(t,r,recoveryInput())["candidateId"].(string)
 second:=recoveryRecord(t,r,recoveryInput())["candidateId"].(string)
 mustExec(t,db,"UPDATE recovery_candidates SET visible_text='tampered' WHERE candidate_id=?",second)
 rows,err:=r.ListPending(ctx);if rows!=nil{t.Fatalf("partial list escaped %v",rows)};recoveryWant(t,err,ErrRecoveryIntegrityFailed)
 _,err=r.UpdatePending(ctx,second,"new");recoveryWant(t,err,ErrRecoveryIntegrityFailed)
 // continued intentionally does not validate content integrity in the original.
 recoveryWant(t,r.Resolve(ctx,second,"continued"),nil)
 mustExec(t,db,"UPDATE recovery_candidates SET source_snapshot='corrupt',source_draft_identity_captured=0 WHERE candidate_id=?",first)
 _,err=r.UpdatePending(ctx,first,"new");recoveryWant(t,err,ErrRecoveryIntegrityFailed)
 recoveryWant(t,r.Resolve(ctx,first,"discarded"),nil)
}
func TestRecoveryReplacementIsolationAndRollback(t *testing.T){
 db,r,_:=recoveryFixture(t);ctx:=context.Background();first:=recoveryRecord(t,r,recoveryInput())["candidateId"].(string)
 input:=recoveryInput();input["replacesCandidateId"]=first
 for _,key:=range []string{"projectId","runId"}{bad:=blueprintCopy(input);bad[key]="other";_,err:=r.Record(ctx,bad);recoveryWant(t,err,ErrRecoveryReplacementInvalid)}
 bad:=blueprintCopy(input);bad["chapterNumber"]=700002;bad["source"]=blueprintData(700002);_,err:=r.Record(ctx,bad);recoveryWant(t,err,ErrRecoveryReplacementInvalid)
 second:=recoveryRecord(t,r,input);if second["replacesCandidateId"]!=first||recoveryCount(t,db)!=2{t.Fatalf("replacement %v",second)}
 // Replacement links do not retire the original pending candidate.
 rows,err:=r.ListPending(ctx);if err!=nil||len(rows)!=2||rows[0]["candidateId"]!=first{t.Fatalf("rowid order %v %v",rows,err)}
 mustExec(t,db,`CREATE TRIGGER recovery_insert_corrupt AFTER INSERT ON recovery_candidates BEGIN UPDATE recovery_candidates SET content_hash='bad' WHERE candidate_id=NEW.candidate_id; END`)
 _,err=r.Record(ctx,recoveryInput());recoveryWant(t,err,ErrRecoveryIntegrityFailed);if recoveryCount(t,db)!=2{t.Fatal("record readback failed to roll back")}
 mustExec(t,db,"DROP TRIGGER recovery_insert_corrupt")
 mustExec(t,db,`CREATE TRIGGER recovery_update_corrupt AFTER UPDATE OF visible_text ON recovery_candidates BEGIN UPDATE recovery_candidates SET content_hash='bad' WHERE candidate_id=NEW.candidate_id; END`)
 _,err=r.UpdatePending(ctx,first,"bad edit");recoveryWant(t,err,ErrRecoveryIntegrityFailed)
 mustExec(t,db,"DROP TRIGGER recovery_update_corrupt")
 rows,err=r.ListPending(ctx);if err!=nil||rows[0]["visibleText"]!="正文😀\u0085"{t.Fatalf("edit rollback %v %v",rows,err)}
 canceled,cancel:=context.WithCancel(ctx);cancel();_,err=r.Record(canceled,recoveryInput());if !errors.Is(err,context.Canceled){t.Fatalf("cancel %v",err)}
 if recoveryCount(t,db)!=2{t.Fatal("canceled record wrote")}
 if _,err:=r.UpdatePending(ctx,first,"after cancellation");err!=nil{t.Fatalf("connection poisoned %v",err)}
 mustExec(t,db,`CREATE TRIGGER recovery_resolve_revert AFTER UPDATE OF status ON recovery_candidates BEGIN UPDATE recovery_candidates SET status='pending',resolved_at=NULL WHERE candidate_id=NEW.candidate_id; END`)
 recoveryWant(t,r.Resolve(ctx,first,"discarded"),ErrRecoveryNotPending)
 mustExec(t,db,"DROP TRIGGER recovery_resolve_revert")
 rows,err=r.ListPending(ctx);if err!=nil||len(rows)!=2{t.Fatalf("resolve rollback %v %v",rows,err)}
}
func TestRecoveryConcurrentResolveSingleWinner(t *testing.T){
 db,r,path:=recoveryFixture(t);db.SetMaxOpenConns(1)
 id:=recoveryRecord(t,r,recoveryInput())["candidateId"].(string)
 other:=NewRecovery(rrSecondPool(t,path));var wg sync.WaitGroup;start:=make(chan struct{});results:=make(chan error,2)
 for _,repo:=range []*Recovery{r,other}{wg.Add(1);go func(repo *Recovery){defer wg.Done();<-start;results<-repo.Resolve(context.Background(),id,"continued")}(repo)}
 close(start);wg.Wait();close(results);wins,losses:=0,0
 for err:=range results{if err==nil{wins++}else if errors.Is(err,ErrRecoveryNotPending){losses++}else{t.Fatal(err)}}
 if wins!=1||losses!=1{t.Fatalf("resolve winners=%d losers=%d",wins,losses)}
}
func TestRecoveryVisibleRegexAndNullishSource(t *testing.T){
 for _,test:=range []struct{input,want string}{
  {"before<think>a\nb</think>after","beforeafter"},{"prefix</think>suffix","suffix"},
  {"A<THINK>hidden", "A"},{"a</think>b</think>c","bc"},{"\ufeff\u0085\ufeff","\u0085"},
  {"<think attr='x'>literal</think>",""},
 }{if got:=recoveryVisible(test.input);got!=test.want{t.Fatalf("visible(%q)=%q want=%q",test.input,got,test.want)}}
 _,r,_:=recoveryFixture(t);input:=recoveryInput();source:=input["source"].(map[string]any);source["suspenseHook"]=nil;delete(source,"userGuidance");source["characters"]=[]string{"阿青","😀"}
 out:=recoveryRecord(t,r,input);if out["sourceCurrent"]!=false{t.Fatalf("different normalized source %v",out)}
}
