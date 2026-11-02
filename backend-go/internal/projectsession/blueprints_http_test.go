package projectsession

import(
 "bytes"
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "os"
 "strings"
 "sync"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)
func TestBlueprintBrowserCRUDRangeReplayAndLease(t *testing.T){
 project,contract,source:=sessionFixture(t);before,err:=os.ReadFile(source);if err!=nil{t.Fatal(err)};file,err:=os.Open("../../../web/operations.json");if err!=nil{t.Fatal(err)};registry,err:=policy.Load(file);file.Close();if err!=nil{t.Fatal(err)};server:=transport.New(registry,security.NewOrigins("http://synthetic.test"),"synthetic");defer server.Close();manager:=New();defer manager.Close();provider:=transport.BlueprintProviderFunc(func(ctx context.Context,c transport.Caller,raw any)(*store.Blueprints,func(),error){expected,err:=ParseContext(raw);if err!=nil{return nil,nil,err};return manager.BeginBlueprints(ctx,Owner{c.SessionID,c.ClientID},expected)});if err:=server.BindBlueprints(provider);err!=nil{t.Fatal(err)};var caller transport.Caller;if err:=server.Bind("config:get",func(_ context.Context,c transport.Caller,_ []any)(any,error){caller=c;return nil,nil});err!=nil{t.Fatal(err)}
 bootstrap:=func()(*http.Cookie,string){request:=httptest.NewRequest("GET","/api/bootstrap",nil);request.Header.Set("X-Novel-User","synthetic-author");response:=httptest.NewRecorder();server.ServeHTTP(response,request);if response.Code!=200{t.Fatal(response.Code)};var value struct{Token string `json:"csrfToken"`};if err:=json.Unmarshal(response.Body.Bytes(),&value);err!=nil{t.Fatal(err)};return response.Result().Cookies()[0],value.Token};cookie,csrf:=bootstrap()
 invoke:=func(cookie *http.Cookie,csrf,tab,op string,args []any)(int,map[string]any,string){data,err:=json.Marshal(map[string]any{"args":args});if err!=nil{t.Fatal(err)};request:=httptest.NewRequest(http.MethodPost,"/api/operations/"+op,bytes.NewReader(data));request.AddCookie(cookie);request.Header.Set("X-Novel-User","synthetic-author");request.Header.Set("X-Client-Id",tab);request.Header.Set("X-CSRF-Token",csrf);request.Header.Set("Origin","http://synthetic.test");request.Header.Set("Content-Type","application/json");response:=httptest.NewRecorder();server.ServeHTTP(response,request);var result map[string]any;if err:=json.Unmarshal(response.Body.Bytes(),&result);err!=nil{t.Fatal(err)};return response.Code,result,response.Body.String()}
 const tab="synthetic-blueprint-tab-0001";if status,_,_:=invoke(cookie,csrf,tab,"config:get",[]any{});status!=200{t.Fatal(status)};owner:=Owner{caller.SessionID,caller.ClientID};expected,err:=manager.OpenScratchCopy(context.Background(),owner,project,contract,t.TempDir(),32*1024*1024);if err!=nil{t.Fatal(err)}
 call:=func(op string,args ...any)any{args=append(args,project.Path(),expected);status,result,raw:=invoke(cookie,csrf,tab,op,args);if status!=200{t.Fatal(op,status,raw)};return result["result"]}
 blueprint:=func(chapter int,title string)map[string]any{return map[string]any{"chapterNumber":chapter,"title":title,"role":"main","purpose":"目的😀","keyEvents":"事件","characters":[]any{"阿青"},"suspenseHook":"悬念","userGuidance":"","notes":"","notesUpdatedAt":""}}
 first:=blueprint(1,"作者原蓝图");first["userGuidance"]="作者指导必须保留";first["notes"]="作者摘要";first["notesUpdatedAt"]="synthetic-fixed-author-clock";if result:=call("db:blueprint-upsert",first).(map[string]any);result["success"]!=true{t.Fatal(result)};if result:=call("db:blueprint-get",1).(map[string]any);result["notes"]!="作者摘要"{t.Fatal(result)}
 if result:=call("db:blueprint-upsert-many",[]any{blueprint(2,"第二章"),blueprint(3,"第三章")}).(map[string]any);result["success"]!=true{t.Fatal(result)}
 request:=map[string]any{"mode":"replace-range","operationId":"synthetic-range-1","startChapter":1,"endChapter":2,"blueprints":[]any{blueprint(1,"范围新标题"),blueprint(2,"范围第二章")}}
 receipt:=call("db:blueprint-commit-range",request).(map[string]any);if receipt["success"]!=true{t.Fatal(receipt)};ack:=receipt["receipt"].(map[string]any);if ack["idempotent"]!=false||len(ack["chapterNumbers"].([]any))!=2{t.Fatal(ack)};snapshot:=ack["snapshot"].([]any);if snapshot[0].(map[string]any)["notes"]!="作者摘要"||snapshot[0].(map[string]any)["notesUpdatedAt"]!="synthetic-fixed-author-clock"||snapshot[0].(map[string]any)["userGuidance"]!="作者指导必须保留"{t.Fatal("lost author group",snapshot)}
 replay:=call("db:blueprint-commit-range",request).(map[string]any);if replay["success"]!=true||replay["receipt"].(map[string]any)["idempotent"]!=true{t.Fatal(replay)};syncOperation:=ack["characterSyncOperation"].(map[string]any);syncID:=syncOperation["operationId"].(string);if pending:=call("db:blueprint-character-sync-list-pending").([]any);len(pending)!=1{t.Fatal(pending)};if operation:=call("db:blueprint-character-sync-get",syncID).(map[string]any);operation["status"]!="pending"{t.Fatal(operation)}
 // Replay is read-only: even a normal direct edit is not silently overwritten.
 edit:=blueprint(1,"后来作者修改");if result:=call("db:blueprint-upsert",edit).(map[string]any);result["success"]!=true{t.Fatal(result)};replay=call("db:blueprint-commit-range",request).(map[string]any);if replay["success"]!=true||replay["receipt"].(map[string]any)["snapshot"].([]any)[0].(map[string]any)["title"]!="后来作者修改"{t.Fatal(replay)}
 for _,field:=range []string{"generationRunHandle","authorRecovery"}{owned:=map[string]any{};for key,value:=range request{owned[key]=value};owned[field]=map[string]any{"synthetic":true};result:=call("db:blueprint-commit-range",owned).(map[string]any);if result["success"]!=false||!strings.Contains(result["error"].(string),"GENERATION_OWNER_REQUIRED"){t.Fatal("unported owner admitted",field,result)}}
 notesDenied:=call("db:blueprint-update-notes",1,"must not commit").(map[string]any);if notesDenied["success"]!=false||!strings.Contains(notesDenied["error"].(string),"GENERATION_FINALIZATION_ADMISSION_REQUIRED"){t.Fatal(notesDenied)}
 if result:=call("db:blueprint-delete",true).(map[string]any);result["success"]!=false{t.Fatal("boolean bound to chapter1",result)};if call("db:blueprint-get",1)==nil{t.Fatal("bool identity deleted chapter")}
 // Concurrent exact replays use the existing receipt, never duplicate the ledger.
 var group sync.WaitGroup;failures:=make(chan string,6);for i:=0;i<6;i++{group.Add(1);go func(){defer group.Done();status,result,raw:=invoke(cookie,csrf,tab,"db:blueprint-commit-range",[]any{request,project.Path(),expected});if status!=200{failures<-raw;return};value,ok:=result["result"].(map[string]any);if !ok||value["success"]!=true{failures<-raw}}()};group.Wait();close(failures);for failure:=range failures{t.Fatal(failure)}
 otherCookie,otherCSRF:=bootstrap();for _,other:=range []struct{cookie *http.Cookie;csrf,tab string}{{cookie,csrf,"synthetic-blueprint-tab-0002"},{otherCookie,otherCSRF,tab}}{if status,_,_:=invoke(other.cookie,other.csrf,other.tab,"db:blueprint-get-all",[]any{project.Path(),expected});status==200{t.Fatal("cross owner blueprint read")}};if status,_,_:=invoke(cookie,"bad",tab,"db:blueprint-delete",[]any{1,project.Path(),expected});status!=403{t.Fatal(status)}
 repository,done,err:=manager.BeginBlueprints(context.Background(),owner,expected);if err!=nil||repository==nil{t.Fatal(err)};if err:=manager.Release(owner,expected);err==nil{t.Fatal("release during blueprint call")};done();if err:=manager.Release(owner,expected);err!=nil{t.Fatal(err)};if status,_,_:=invoke(cookie,csrf,tab,"db:blueprint-get-all",[]any{project.Path(),expected});status==200{t.Fatal("stale context")};after,err:=os.ReadFile(source);if err!=nil||!bytes.Equal(before,after){t.Fatal("source mutated",err)}
}
