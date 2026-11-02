package transport

import (
 "context"
 "errors"
 "net/http"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
type PlotTreeProvider interface{BeginPlotTree(context.Context,Caller,any)(*store.PlotTree,func(),error)}
type PlotTreeProviderFunc func(context.Context,Caller,any)(*store.PlotTree,func(),error)
func(f PlotTreeProviderFunc)BeginPlotTree(ctx context.Context,c Caller,expected any)(*store.PlotTree,func(),error){if f==nil{return nil,nil,errors.New("PLOT_TREE_PROVIDER_REQUIRED")};return f(ctx,c,expected)}
func(s *Server)BindPlotTree(provider PlotTreeProvider)error{
 if provider==nil{return errors.New("PLOT_TREE_PROVIDER_REQUIRED")};names:=[]string{"db:plot-tree-read","db:plot-tree-save","db:plot-tree-clear"};handlers:=make(map[string]Operation,len(names))
 for _,name:=range names{handlers[name]=func(ctx context.Context,c Caller,args []any)(any,error){
  fail:=func(err error)(any,error){code:=plotTreeCode(err);if name=="db:plot-tree-save"{return map[string]any{"success":false,"error":"Error: "+code},nil};var public *security.HTTPError;if errors.As(err,&public){return nil,public};return nil,errors.New(code)}
  arity,pathIndex:=2,0;if name=="db:plot-tree-save"{arity,pathIndex=4,2};if len(args)!=arity{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};expected,err:=draftExpectedContext(args,len(args)-1);if err!=nil{return fail(err)};path,ok:=args[pathIndex].(string);if !ok||path==""{return fail(security.NewHTTPError(http.StatusBadRequest,"PROJECT_CONTEXT_REQUIRED"))};if path!=expected["projectPath"]{return fail(security.NewHTTPError(http.StatusConflict,"PROJECT_CONTEXT_CHANGED"))}
  repository,release,err:=provider.BeginPlotTree(ctx,c,expected);if release!=nil{defer release()};if err!=nil{return fail(err)};if repository==nil||release==nil{return fail(errors.New("PLOT_TREE_PROVIDER_UNAVAILABLE"))}
  // Original public controller deliberately refuses renderer-generated writes.
  // Do not invoke repository.Save or validate/rebase onto current server facts.
  if name=="db:plot-tree-save"{return fail(errors.New("GENERATION_GRAPH_ADMISSION_REQUIRED"))};if name=="db:plot-tree-clear"{if err:=repository.Clear(ctx);err!=nil{return fail(err)};return map[string]any{"success":true},nil};result,err:=repository.Read(ctx);if err!=nil{return fail(err)};return reviewRevisionWire(result),nil
 }}
 s.mu.Lock();defer s.mu.Unlock();for _,name:=range names{if _,ok:=s.registry.Lookup(name);!ok{return errors.New("OPERATION_NOT_EXPOSED")};if _,ok:=s.handlers[name];ok{return errors.New("OPERATION_ALREADY_BOUND")}};for name,handler:=range handlers{s.handlers[name]=handler};return nil
}
func plotTreeCode(err error)string{var public *security.HTTPError;if errors.As(err,&public){return public.Code};if err!=nil{switch err.Error(){case "PROJECT_NOT_OPEN","PLOT_TREE_PROVIDER_UNAVAILABLE","PLOT_TREE_PROJECT_CORE_MISSING","PLOT_TREE_CLEAR_FAILED","GENERATION_GRAPH_ADMISSION_REQUIRED":return err.Error()}};return "PLOT_TREE_OPERATION_FAILED"}
