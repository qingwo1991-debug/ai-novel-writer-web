package transport

import(
 "context"
 "errors"
 "net/http"
 "strings"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
type BlueprintProvider interface{BeginBlueprints(context.Context,Caller,any)(*store.Blueprints,func(),error)}
type BlueprintProviderFunc func(context.Context,Caller,any)(*store.Blueprints,func(),error)
func(f BlueprintProviderFunc)BeginBlueprints(ctx context.Context,c Caller,expected any)(*store.Blueprints,func(),error){if f==nil{return nil,nil,errors.New("BLUEPRINT_PROVIDER_REQUIRED")};return f(ctx,c,expected)}
// BindBlueprints deliberately excludes clear-all and completion of character sync:
// neither generation recovery retirement nor authoritative roster proof is ported.
func(s *Server)BindBlueprints(provider BlueprintProvider)error{
 if provider==nil{return errors.New("BLUEPRINT_PROVIDER_REQUIRED")};names:=[]string{"db:blueprint-get-all","db:blueprint-get","db:blueprint-upsert","db:blueprint-upsert-many","db:blueprint-delete","db:blueprint-commit-range","db:blueprint-character-sync-list-pending","db:blueprint-character-sync-get","db:blueprint-update-notes"};handlers:=make(map[string]Operation,len(names))
 for _,name:=range names{handlers[name]=func(ctx context.Context,c Caller,args []any)(any,error){
  caught:=name!="db:blueprint-get-all"&&name!="db:blueprint-get"&&name!="db:blueprint-character-sync-list-pending"&&name!="db:blueprint-character-sync-get"
  fail:=func(err error)(any,error){code:=blueprintCode(err);if caught{return map[string]any{"success":false,"error":"Error: "+code},nil};var public *security.HTTPError;if errors.As(err,&public){return nil,public};return nil,errors.New(code)}
  arity,pathIndex:=3,1;switch name{case "db:blueprint-get-all","db:blueprint-character-sync-list-pending":arity,pathIndex=2,0;case "db:blueprint-update-notes":arity,pathIndex=4,2};if len(args)!=arity{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};expected,err:=draftExpectedContext(args,len(args)-1);if err!=nil{return fail(err)};path,ok:=args[pathIndex].(string);if !ok||path==""{return fail(security.NewHTTPError(http.StatusBadRequest,"PROJECT_CONTEXT_REQUIRED"))};if path!=expected["projectPath"]{return fail(security.NewHTTPError(http.StatusConflict,"PROJECT_CONTEXT_CHANGED"))}
  var input map[string]any;var batch []map[string]any;var syncID string
  switch name{case "db:blueprint-upsert","db:blueprint-commit-range":input,ok=args[0].(map[string]any);if !ok||input==nil{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};case "db:blueprint-upsert-many":items,ok:=args[0].([]any);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};batch=make([]map[string]any,len(items));for i,item:=range items{batch[i],ok=item.(map[string]any);if !ok||batch[i]==nil{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}};case "db:blueprint-character-sync-get":syncID,ok=args[0].(string);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}}
  repository,release,err:=provider.BeginBlueprints(ctx,c,expected);if release!=nil{defer release()};if err!=nil{return fail(err)};if repository==nil||release==nil{return fail(errors.New("BLUEPRINT_PROVIDER_UNAVAILABLE"))};var result any
  switch name{case "db:blueprint-get-all":result,err=repository.All(ctx);case "db:blueprint-get":result,err=repository.Get(ctx,args[0]);case "db:blueprint-upsert":err=repository.Upsert(ctx,input);case "db:blueprint-upsert-many":err=repository.UpsertMany(ctx,batch);case "db:blueprint-delete":err=repository.Delete(ctx,args[0]);case "db:blueprint-commit-range":result,err=repository.CommitRange(ctx,input);case "db:blueprint-character-sync-list-pending":result,err=repository.PendingSync(ctx);case "db:blueprint-character-sync-get":result,err=repository.GetSync(ctx,syncID);case "db:blueprint-update-notes":return fail(errors.New("GENERATION_FINALIZATION_ADMISSION_REQUIRED"))}
  if err!=nil{return fail(err)};switch name{case "db:blueprint-commit-range":return map[string]any{"success":true,"receipt":reviewRevisionWire(result)},nil;case "db:blueprint-upsert","db:blueprint-upsert-many","db:blueprint-delete":return map[string]any{"success":true},nil};return reviewRevisionWire(result),nil
 }}
 s.mu.Lock();defer s.mu.Unlock();for _,name:=range names{if _,ok:=s.registry.Lookup(name);!ok{return errors.New("OPERATION_NOT_EXPOSED")};if _,ok:=s.handlers[name];ok{return errors.New("OPERATION_ALREADY_BOUND")}};for name,handler:=range handlers{s.handlers[name]=handler};return nil
}
func blueprintCode(err error)string{var public *security.HTTPError;if errors.As(err,&public){return public.Code};if err!=nil{code:=err.Error();if strings.HasPrefix(code,"BLUEPRINT_"){switch code{case "BLUEPRINT_OPERATION_ID_REQUIRED","BLUEPRINT_COMMIT_MODE_INVALID","BLUEPRINT_COMMIT_RANGE_INVALID","BLUEPRINT_COMMIT_RANGE_MISMATCH","BLUEPRINT_COMMIT_RANGE_INCOMPLETE","BLUEPRINT_COMMIT_RECEIPT_INVALID","BLUEPRINT_COMMIT_CANONICAL_KEY_UNSUPPORTED","BLUEPRINT_CHARACTER_SYNC_STATE_INVALID","BLUEPRINT_COMMIT_READ_BACK_FAILED","BLUEPRINT_OPERATION_CONFLICT","BLUEPRINT_OPERATION_FACTS_CHANGED","BLUEPRINT_CHARACTER_SYNC_OPERATION_MISSING","BLUEPRINT_CHARACTER_SYNC_OPERATION_ID_REQUIRED","BLUEPRINT_CHARACTER_SYNC_INPUT_INVALID","BLUEPRINT_CHARACTER_SYNC_RECEIPT_INVALID","BLUEPRINT_PROVIDER_UNAVAILABLE":return code}};switch code{case "PROJECT_NOT_OPEN","GENERATION_OWNER_REQUIRED","GENERATION_FINALIZATION_ADMISSION_REQUIRED":return code}};return "BLUEPRINT_OPERATION_FAILED"}
