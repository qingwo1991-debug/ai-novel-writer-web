package projectsession

import (
 "bytes"
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "os"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)
func TestPostHistoryBrowserLedgerFinalizedIdentityAndUnknownUsage(t *testing.T){
 project,contract,source:=sessionFixture(t)
 // Source synthetic fixture initialization only. Production author data is never
 // opened; every browser mutation below goes to the admitted disposable copy.
 dsn,err:=store.ExistingDSN(source);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};var body string;if err:=db.QueryRow("SELECT body FROM contents WHERE id=1").Scan(&body);err!=nil{db.Close();t.Fatal(err)};hash:=sha256.Sum256([]byte(body));contentHash:=hex.EncodeToString(hash[:]);if _,err:=db.Exec("UPDATE drafts SET status='finalized' WHERE id=1");err!=nil{db.Close();t.Fatal(err)};if _,err:=db.Exec(`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_revision,content_snapshot,target_file_name) VALUES('synthetic-finalization',1,1,'public title',?,1,?,'public.txt')`,contentHash,body);err!=nil{db.Close();t.Fatal(err)};if err:=db.Close();err!=nil{t.Fatal(err)}
 before,err:=os.ReadFile(source);if err!=nil{t.Fatal(err)};file,err:=os.Open("../../../web/operations.json");if err!=nil{t.Fatal(err)};registry,err:=policy.Load(file);file.Close();if err!=nil{t.Fatal(err)}
 server:=transport.New(registry,security.NewOrigins("http://synthetic.test"),"synthetic");defer server.Close();manager:=New();defer manager.Close();provider:=transport.PostHistoryProviderFunc(func(ctx context.Context,c transport.Caller,raw any)(*store.PostHistory,func(),error){expected,err:=ParseContext(raw);if err!=nil{return nil,nil,err};return manager.BeginPostHistory(ctx,Owner{SessionID: c.SessionID, ClientID: c.ClientID},expected)})
 if err:=server.BindPostHistory(provider);err!=nil{t.Fatal(err)};if err:=server.BindPostHistory(provider);err==nil{t.Fatal("duplicate binding replaced handlers")};var caller transport.Caller;if err:=server.Bind("config:get",func(_ context.Context,c transport.Caller,_ []any)(any,error){caller=c;return nil,nil});err!=nil{t.Fatal(err)}
 bootstrap:=func()(*http.Cookie,string){request:=httptest.NewRequest("GET","/api/bootstrap",nil);request.Header.Set("X-Novel-User","synthetic-author");response:=httptest.NewRecorder();server.ServeHTTP(response,request);if response.Code!=200{t.Fatal(response.Code)};var result struct{Token string `json:"csrfToken"`};if json.Unmarshal(response.Body.Bytes(),&result)!=nil{t.Fatal("bootstrap")};return response.Result().Cookies()[0],result.Token};cookie,csrf:=bootstrap()
 invoke:=func(cookie *http.Cookie,csrf,tab,op string,args []any)(int,map[string]any,string){data,err:=json.Marshal(map[string]any{"args":args});if err!=nil{t.Fatal(err)};request:=httptest.NewRequest(http.MethodPost,"/api/operations/"+op,bytes.NewReader(data));request.AddCookie(cookie);request.Header.Set("X-Novel-User","synthetic-author");request.Header.Set("X-Client-Id",tab);request.Header.Set("X-CSRF-Token",csrf);request.Header.Set("Origin","http://synthetic.test");request.Header.Set("Content-Type","application/json");response:=httptest.NewRecorder();server.ServeHTTP(response,request);var result map[string]any;if json.Unmarshal(response.Body.Bytes(),&result)!=nil{t.Fatal(response.Body.String())};return response.Code,result,response.Body.String()}
 const tab="synthetic-posthistory-tab-0001";if status,_,_:=invoke(cookie,csrf,tab,"config:get",[]any{});status!=200{t.Fatal(status)};owner:=Owner{SessionID: caller.SessionID, ClientID: caller.ClientID};expected,err:=manager.OpenScratchCopy(context.Background(),owner,project,contract,t.TempDir(),32*1024*1024);if err!=nil{t.Fatal(err)}
 call:=func(op string,args ...any)map[string]any{args=append(args,project.Path(),expected);status,result,_:=invoke(cookie,csrf,tab,op,args);if status!=200{t.Fatal(op,status,result)};return result}
 business:=func(result map[string]any)map[string]any{data,ok:=result["result"].(map[string]any);if !ok||data["success"]!=true{t.Fatal("business failure",result)};return data}
 identity:=map[string]any{"draftId":1,"chapterNumber":1,"finalizationId":"synthetic-finalization","contentHash":contentHash}
 input:=map[string]any{"triggerSourceType":"chapter_finalize","triggerSourceId":"1","sourceLabel":"synthetic finalization","finalizedSource":identity,"steps":[]any{map[string]any{"key":"knowledge","label":"Knowledge projection","critical":true},map[string]any{"key":"optional","label":"Optional projection","critical":false}}}
 runID:=business(call("db:post-process-create-run",input))["id"].(string);if len(runID)!=36||runID[14]!='4'{t.Fatal("not a secure random v4 UUID",runID)}
 latest:=call("db:post-process-get-latest-run","chapter_finalize","1")["result"].(map[string]any);if latest["id"]!=runID||latest["triggerSourceId"]!="finalization:synthetic-finalization"||latest["allCriticalPassed"]!=false{t.Fatal(latest)}
 business(call("db:post-process-mark-step-ok",runID,"knowledge"));if passed:=call("db:post-process-is-all-passed","chapter_finalize","1")["result"];passed!=true{t.Fatal("atomic aggregate not advanced",passed)}
 business(call("db:post-process-mark-step-failed",runID,"optional","optional failure"));if call("db:post-process-is-all-passed","chapter_finalize","1")["result"]!=true{t.Fatal("optional failure changed critical aggregate")}
 business(call("db:post-process-mark-step-failed",runID,"knowledge","retry needed"));if call("db:post-process-is-all-passed","chapter_finalize","1")["result"]!=false{t.Fatal("critical retry retained stale success")}
 steps:=call("db:post-process-get-steps",runID)["result"].([]any);step:=steps[0].(map[string]any);if step["attemptCount"]!=float64(2)||step["completedAt"]!=""||step["errorMsg"]!="retry needed"{t.Fatal(step)}
 missing:=call("db:post-process-mark-step-ok",runID,"absent")["result"].(map[string]any);if missing["error"]!="Error: POST_PROCESS_STEP_NOT_FOUND"{t.Fatal(missing)}
 identity["contentHash"]=strings.Repeat("0",64);changed:=call("db:post-process-create-run",input)["result"].(map[string]any);if changed["error"]!="Error: POST_PROCESS_FINALIZATION_SOURCE_CHANGED"{t.Fatal(changed)};identity["contentHash"]=contentHash
 unowned:=map[string]any{"triggerSourceType":"chapter_finalize","triggerSourceId":"finalization:synthetic-finalization","sourceLabel":"no frozen proof","steps":[]any{}};failed:=call("db:post-process-create-run",unowned)["result"].(map[string]any);if failed["error"]!="Error: POST_PROCESS_FINALIZATION_SOURCE_REQUIRED"{t.Fatal(failed)}
 unknown:=map[string]any{"modelId":"synthetic-model","modelName":"Synthetic","purpose":"review","promptTokens":nil,"completionTokens":nil,"totalTokens":nil,"durationMs":3,"success":false,"errorMessage":"private upstream prompt must not leave history"};business(call("db:log-llm-call",unknown));stats:=call("db:get-llm-stats")["result"].(map[string]any);if stats["knownUsageCalls"]!=float64(0)||stats["totalTokens"]!=nil||stats["totalPromptTokens"]!=nil{t.Fatal("unknown usage coerced to zero",stats)}
 known:=map[string]any{"modelId":"synthetic-model","modelName":"Synthetic","purpose":"write","promptTokens":0,"completionTokens":0,"totalTokens":0,"durationMs":4,"success":true};business(call("db:log-llm-call",known));stats=call("db:get-llm-stats")["result"].(map[string]any);if stats["knownUsageCalls"]!=float64(1)||stats["totalTokens"]!=float64(0)||stats["failedCalls"]!=float64(1)||stats["successfulCalls"]!=float64(1){t.Fatal(stats)}
 history:=call("db:get-llm-history",nil)["result"].([]any);if len(history)!=2||history[0].(map[string]any)["finishReason"]!="stop"||history[1].(map[string]any)["finishReason"]!=nil{t.Fatal(history)};serialized,_:=json.Marshal(history);if strings.Contains(string(serialized),"private upstream")||strings.Contains(string(serialized),"errorMessage"){t.Fatal("private raw diagnostics exposed",string(serialized))};if len(call("db:get-llm-history",0)["result"].([]any))!=0{t.Fatal("limit zero changed")}
 otherCookie,otherCSRF:=bootstrap();for _,other:=range []struct{cookie *http.Cookie;csrf,tab string}{{cookie,csrf,"synthetic-posthistory-tab-0002"},{otherCookie,otherCSRF,tab}}{status,_,_:=invoke(other.cookie,other.csrf,other.tab,"db:get-llm-stats",[]any{project.Path(),expected});if status==200{t.Fatal("cross-session/tab read leaked")}}
 if status,_,_:=invoke(cookie,"invalid-csrf",tab,"db:post-process-mark-step-ok",[]any{runID,"knowledge",project.Path(),expected});status!=403{t.Fatal("CSRF missing gate",status)}
 repository,done,err:=manager.BeginPostHistory(context.Background(),owner,expected);if err!=nil||repository==nil{t.Fatal(err)};if err:=manager.Release(owner,expected);err==nil{t.Fatal("released active ledger")};done();if err:=manager.Release(owner,expected);err!=nil{t.Fatal(err)};if status,_,_:=invoke(cookie,csrf,tab,"db:get-llm-stats",[]any{project.Path(),expected});status==200{t.Fatal("stale history context reused")}
 after,err:=os.ReadFile(source);if err!=nil||!bytes.Equal(after,before){t.Fatal("source fixture changed",err)}
}
