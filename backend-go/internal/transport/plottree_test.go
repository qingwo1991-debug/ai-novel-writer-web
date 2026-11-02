package transport

import (
 "context"
 "errors"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
func TestPlotTreeBindingsAndGenerationAdmissionGate(t *testing.T){
 server:=fixture(t);provider:=PlotTreeProviderFunc(func(context.Context,Caller,any)(*store.PlotTree,func(),error){return store.NewPlotTree(nil),func(){},nil})
 if err:=server.Bind("db:plot-tree-clear",func(context.Context,Caller,[]any)(any,error){return "existing",nil});err!=nil{t.Fatal(err)};if err:=server.BindPlotTree(provider);err==nil{t.Fatal("partial bind allowed")};if _,ok:=server.handlers["db:plot-tree-read"];ok{t.Fatal("failed bind partially registered")}
 server=fixture(t);if err:=server.BindPlotTree(provider);err!=nil{t.Fatal(err)};cookie,csrf:=bootstrap(t,server);expected:=draftContext("synthetic-project","synthetic-lease","/synthetic/book")
 response:=draftRPC(t,server,cookie,csrf,"synthetic-plot-tab-001","db:plot-tree-save",nil,nil,"/synthetic/book",expected);if response.Code!=200||!strings.Contains(response.Body.String(),"GENERATION_GRAPH_ADMISSION_REQUIRED"){t.Fatal(response.Code,response.Body.String())}
 provider=PlotTreeProviderFunc(func(context.Context,Caller,any)(*store.PlotTree,func(),error){return nil,nil,errors.New("upstream private prompt /author/path")});server=fixture(t);if err:=server.BindPlotTree(provider);err!=nil{t.Fatal(err)};cookie,csrf=bootstrap(t,server);response=draftRPC(t,server,cookie,csrf,"synthetic-plot-tab-001","db:plot-tree-read","/synthetic/book",expected);if response.Code!=500||!strings.Contains(response.Body.String(),"PLOT_TREE_OPERATION_FAILED")||strings.Contains(response.Body.String(),"/author"){t.Fatal(response.Code,response.Body.String())}
}
