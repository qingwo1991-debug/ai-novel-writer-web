package transport

import (
 "context"
 "errors"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
func TestPostHistoryAtomicBindingsAndPrivateErrors(t *testing.T){
 server:=fixture(t);providerCalls:=0;provider:=PostHistoryProviderFunc(func(context.Context,Caller,any)(*store.PostHistory,func(),error){providerCalls++;return nil,nil,errors.New("private upstream payload /author/path")})
 if err:=server.Bind("db:get-llm-stats",func(context.Context,Caller,[]any)(any,error){return "existing",nil});err!=nil{t.Fatal(err)};if err:=server.BindPostHistory(provider);err==nil{t.Fatal("partial bind allowed")};if _,ok:=server.handlers["db:post-process-create-run"];ok{t.Fatal("binding failure partially registered")}
 server=fixture(t);if err:=server.BindPostHistory(provider);err!=nil{t.Fatal(err)};cookie,csrf:=bootstrap(t,server);expected:=draftContext("synthetic-project","synthetic-lease","/synthetic/book")
 response:=draftRPC(t,server,cookie,csrf,"synthetic-post-tab-001","db:get-llm-history","not a limit","/synthetic/book",expected);if response.Code!=400||providerCalls!=0{t.Fatal(response.Code,response.Body.String(),providerCalls)}
 response=draftRPC(t,server,cookie,csrf,"synthetic-post-tab-001","db:post-process-create-run",map[string]any{},"/synthetic/book",expected);if response.Code!=200||!strings.Contains(response.Body.String(),"POST_HISTORY_OPERATION_FAILED")||strings.Contains(response.Body.String(),"/author/"){t.Fatal(response.Code,response.Body.String())}
 response=draftRPC(t,server,cookie,csrf,"synthetic-post-tab-001","db:log-llm-call",map[string]any{},"/synthetic/book",expected);if response.Code!=200||!strings.Contains(response.Body.String(),`"success":false`)||strings.Contains(response.Body.String(),"payload")||strings.Contains(response.Body.String(),"error"){t.Fatal(response.Code,response.Body.String())}
}
