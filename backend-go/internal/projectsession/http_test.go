package projectsession

import (
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
// This is a complete in-process authenticated HTTP -> frozen context -> lease ->
// admitted private SQLite -> repository CAS test. No fake-success repository and
// no author project writes. It deliberately does NOT claim project:open exists.
func TestBrowserWireDraftCASOnAdmittedScratchSession(t *testing.T){
 project,contract,source:=sessionFixture(t);original,err:=os.ReadFile(source);if err!=nil{t.Fatal(err)}
 file,err:=os.Open("../../../web/operations.json");if err!=nil{t.Fatal(err)};registry,err:=policy.Load(file);file.Close();if err!=nil{t.Fatal(err)}
 server:=transport.New(registry,security.NewOrigins("http://synthetic.test"),"synthetic");defer server.Close()
 manager:=New();defer manager.Close()
 if err:=server.BindDrafts(transport.DraftProviderFunc(func(ctx context.Context,caller transport.Caller,raw any)(*store.Drafts,func(),error){expected,err:=ParseContext(raw);if err!=nil{return nil,nil,err};return manager.Begin(ctx,Owner{caller.SessionID,caller.ClientID},expected)}));err!=nil{t.Fatal(err)}
 // Capture the authenticated server-side caller without exposing it in an HTTP
 // response. This test-only handler is not registered by the application binary.
 var caller transport.Caller
 if err:=server.Bind("config:get",func(_ context.Context,value transport.Caller,_ []any)(any,error){caller=value;return nil,nil});err!=nil{t.Fatal(err)}
 boot:=httptest.NewRequest("GET","/api/bootstrap",nil);boot.Header.Set("X-Novel-User","synthetic-author");response:=httptest.NewRecorder();server.ServeHTTP(response,boot)
 if response.Code!=200{t.Fatal(response.Code,response.Body.String())};cookie:=response.Result().Cookies()[0]
 var bootstrap struct{CSRFToken string `json:"csrfToken"`};if err:=json.Unmarshal(response.Body.Bytes(),&bootstrap);err!=nil{t.Fatal(err)}
 invoke:=func(op,tab string,args []any)(int,map[string]any){
  body,err:=json.Marshal(map[string]any{"args":args});if err!=nil{t.Fatal(err)}
  request:=httptest.NewRequest(http.MethodPost,"/api/operations/"+op,bytes.NewReader(body));request.AddCookie(cookie);request.Header.Set("X-Novel-User","synthetic-author");request.Header.Set("X-Client-Id",tab);request.Header.Set("X-CSRF-Token",bootstrap.CSRFToken);request.Header.Set("Origin","http://synthetic.test");request.Header.Set("Content-Type","application/json")
  response:=httptest.NewRecorder();server.ServeHTTP(response,request);var result map[string]any;if err:=json.Unmarshal(response.Body.Bytes(),&result);err!=nil{t.Fatal(response.Body.String(),err)};return response.Code,result
 }
 const tab="synthetic-browser-tab-0001"
 if status,_:=invoke("config:get",tab,[]any{});status!=200{t.Fatal(status)}
 expected,err:=manager.OpenScratchCopy(context.Background(),Owner{caller.SessionID,caller.ClientID},project,contract,t.TempDir(),32*1024*1024);if err!=nil{t.Fatal(err)}
 // ipc-client.ts appends context AFTER the original typed arguments. In
 // particular expectedContent remains argument4, not shifted by the context.
 status,result:=invoke("db:draft-get-full",tab,[]any{1,project.Path(),expected});if status!=200{t.Fatal(status,result)}
 full,ok:=result["result"].(map[string]any);if !ok{t.Fatal(result)};baseline,ok:=full["content"].(string);if !ok{t.Fatal(full)}
 if status,result:=invoke("db:draft-get-full","synthetic-browser-tab-0002",[]any{1,project.Path(),expected});status==200{t.Fatal("cross-tab read allowed",result)}
 status,result=invoke("db:draft-list-all",tab,[]any{project.Path(),expected});if status!=200{t.Fatal(status,result)}
 var wg sync.WaitGroup;var mu sync.Mutex;wins,conflicts:=0,0
 for _,body:=range []string{"first browser body 中文","second browser body 😀"}{wg.Go(func(){
  status,result:=invoke("db:draft-update-content",tab,[]any{1,body,7,project.Path(),baseline,expected});if status!=200{t.Errorf("CAS HTTP %d: %+v",status,result);return}
  value,ok:=result["result"].(map[string]any);if !ok{t.Errorf("CAS envelope %+v",result);return};mu.Lock();defer mu.Unlock();if value["success"]==true{wins++}else if value["errorCode"]=="DRAFT_CONTENT_CONFLICT"{conflicts++}else{t.Errorf("unexpected CAS %+v",value)}
 })};wg.Wait();if wins!=1||conflicts!=1{t.Fatal("full-body CAS lost update",wins,conflicts)}
 status,result=invoke("db:draft-get-full",tab,[]any{1,project.Path(),expected});if status!=200{t.Fatal(status,result)};encoded,_:=json.Marshal(result);if !strings.Contains(string(encoded),"browser body"){t.Fatal(result)}
 after,err:=os.ReadFile(source);if err!=nil||!bytes.Equal(original,after){t.Fatal("HTTP mutated original project",err)}
}
