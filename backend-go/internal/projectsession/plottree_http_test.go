package projectsession

import (
 "bytes"
 "context"
 "database/sql"
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
func TestPlotTreeBrowserSourcesClearAndDeniedRendererSave(t *testing.T){
 project,contract,source:=sessionFixture(t);dsn,err:=store.ExistingDSN(source);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};if _,err:=db.Exec(`UPDATE project_core SET synopsis='原始合成梗概 <&> 😀',plot_tree_snapshot='{broken' WHERE id='main'; INSERT INTO blueprints(chapter_number,title,purpose,key_events)VALUES(1,'第一章','synthetic purpose','事件')`);err!=nil{db.Close();t.Fatal(err)};if err:=db.Close();err!=nil{t.Fatal(err)};before,err:=os.ReadFile(source);if err!=nil{t.Fatal(err)}
 file,err:=os.Open("../../../web/operations.json");if err!=nil{t.Fatal(err)};registry,err:=policy.Load(file);file.Close();if err!=nil{t.Fatal(err)};server:=transport.New(registry,security.NewOrigins("http://synthetic.test"),"synthetic");defer server.Close();manager:=New();defer manager.Close();provider:=transport.PlotTreeProviderFunc(func(ctx context.Context,c transport.Caller,raw any)(*store.PlotTree,func(),error){expected,err:=ParseContext(raw);if err!=nil{return nil,nil,err};return manager.BeginPlotTree(ctx,Owner{c.SessionID,c.ClientID},expected)});if err:=server.BindPlotTree(provider);err!=nil{t.Fatal(err)};var caller transport.Caller;if err:=server.Bind("config:get",func(_ context.Context,c transport.Caller,_ []any)(any,error){caller=c;return nil,nil});err!=nil{t.Fatal(err)}
 bootstrap:=func()(*http.Cookie,string){request:=httptest.NewRequest("GET","/api/bootstrap",nil);request.Header.Set("X-Novel-User","synthetic-author");response:=httptest.NewRecorder();server.ServeHTTP(response,request);if response.Code!=200{t.Fatal(response.Code)};var result struct{Token string `json:"csrfToken"`};if json.Unmarshal(response.Body.Bytes(),&result)!=nil{t.Fatal("bootstrap")};return response.Result().Cookies()[0],result.Token};cookie,csrf:=bootstrap()
 invoke:=func(cookie *http.Cookie,csrf,tab,op string,args []any)(int,map[string]any,string){data,err:=json.Marshal(map[string]any{"args":args});if err!=nil{t.Fatal(err)};request:=httptest.NewRequest(http.MethodPost,"/api/operations/"+op,bytes.NewReader(data));request.AddCookie(cookie);request.Header.Set("X-Novel-User","synthetic-author");request.Header.Set("X-Client-Id",tab);request.Header.Set("X-CSRF-Token",csrf);request.Header.Set("Origin","http://synthetic.test");request.Header.Set("Content-Type","application/json");response:=httptest.NewRecorder();server.ServeHTTP(response,request);var result map[string]any;if json.Unmarshal(response.Body.Bytes(),&result)!=nil{t.Fatal(response.Body.String())};return response.Code,result,response.Body.String()}
 const tab="synthetic-plottree-tab-0001";if status,_,_:=invoke(cookie,csrf,tab,"config:get",[]any{});status!=200{t.Fatal(status)};owner:=Owner{caller.SessionID,caller.ClientID};expected,err:=manager.OpenScratchCopy(context.Background(),owner,project,contract,t.TempDir(),32*1024*1024);if err!=nil{t.Fatal(err)}
 call:=func(op string,args ...any)map[string]any{args=append(args,project.Path(),expected);status,result,_:=invoke(cookie,csrf,tab,op,args);if status!=200{t.Fatal(op,status,result)};return result};first:=call("db:plot-tree-read")["result"].(map[string]any);if first["snapshot"]!=nil||first["storedSnapshotInvalid"]!=true||len(first["blueprints"].([]any))!=1{t.Fatal(first)};revision:=first["sourceRevision"].(string);if len(revision)!=64{t.Fatal(revision)}
 for _,input:=range []any{nil,map[string]any{"version":1,"sourceRevision":revision,"tracks":[]any{}}}{denied:=call("db:plot-tree-save",input,revision)["result"].(map[string]any);if denied["success"]!=false||denied["error"]!="Error: GENERATION_GRAPH_ADMISSION_REQUIRED"{t.Fatal("browser bypassed graph admission",denied)}}
 again:=call("db:plot-tree-read")["result"].(map[string]any);if again["storedSnapshotInvalid"]!=true||again["sourceRevision"]!=revision{t.Fatal("denied save mutated facts",again)}
 cleared:=call("db:plot-tree-clear")["result"].(map[string]any);if cleared["success"]!=true{t.Fatal(cleared)};last:=call("db:plot-tree-read")["result"].(map[string]any);if last["snapshot"]!=nil||last["storedSnapshotInvalid"]!=nil||last["sourceRevision"]!=revision{t.Fatal("explicit clear altered original facts",last)}
 otherCookie,otherCSRF:=bootstrap();for _,other:=range []struct{cookie *http.Cookie;csrf,tab string}{{cookie,csrf,"synthetic-plottree-tab-0002"},{otherCookie,otherCSRF,tab}}{if status,_,_:=invoke(other.cookie,other.csrf,other.tab,"db:plot-tree-read",[]any{project.Path(),expected});status==200{t.Fatal("cross-owner plot read")}}
 if status,_,_:=invoke(cookie,"invalid",tab,"db:plot-tree-clear",[]any{project.Path(),expected});status!=403{t.Fatal("CSRF clear missing",status)}
 repository,done,err:=manager.BeginPlotTree(context.Background(),owner,expected);if err!=nil||repository==nil{t.Fatal(err)};if err:=manager.Release(owner,expected);err==nil{t.Fatal("released active plot hold")};done();if err:=manager.Release(owner,expected);err!=nil{t.Fatal(err)};if status,_,raw:=invoke(cookie,csrf,tab,"db:plot-tree-save",[]any{nil,nil,project.Path(),expected});status==200&&strings.Contains(raw,"GENERATION_GRAPH_ADMISSION_REQUIRED"){t.Fatal("stale lease got valid generation denial",raw)}
 after,err:=os.ReadFile(source);if err!=nil||!bytes.Equal(before,after){t.Fatal("source mutated",err)}
}
