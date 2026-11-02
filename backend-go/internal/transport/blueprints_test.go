package transport

import(
 "context"
 "errors"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
func TestBlueprintBindingAtomicAndPrivateErrors(t *testing.T){
 provider:=BlueprintProviderFunc(func(context.Context,Caller,any)(*store.Blueprints,func(),error){return nil,nil,errors.New("private author path /project.db prompt")});server:=fixture(t);if err:=server.Bind("db:blueprint-upsert",func(context.Context,Caller,[]any)(any,error){return nil,nil});err!=nil{t.Fatal(err)};if err:=server.BindBlueprints(provider);err==nil{t.Fatal("collision accepted")};if _,ok:=server.handlers["db:blueprint-get-all"];ok{t.Fatal("partially bound")}
 server=fixture(t);if err:=server.BindBlueprints(provider);err!=nil{t.Fatal(err)};cookie,csrf:=bootstrap(t,server);expected:=draftContext("synthetic-project","synthetic-lease","/synthetic/book");for _,op:=range []string{"db:blueprint-get-all","db:blueprint-upsert"}{args:=[]any{"/synthetic/book",expected};if op=="db:blueprint-upsert"{args=append([]any{map[string]any{"chapterNumber":1}},args...)};response:=draftRPC(t,server,cookie,csrf,"synthetic-blueprint-tab-1",op,args...);if !strings.Contains(response.Body.String(),"BLUEPRINT_OPERATION_FAILED")||strings.Contains(response.Body.String(),"/project.db"){t.Fatal(response.Code,response.Body.String())};if op=="db:blueprint-get-all"&&response.Code!=500{t.Fatal(response.Code)};if op=="db:blueprint-upsert"&&response.Code!=200{t.Fatal(response.Code)}}
 for _,name:=range []string{"db:blueprint-clear-all","db:blueprint-character-sync-complete"}{if _,ok:=server.handlers[name];ok{t.Fatal("unported authority bound",name)}}
}
