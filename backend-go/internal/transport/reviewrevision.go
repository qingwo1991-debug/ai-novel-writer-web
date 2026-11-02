package transport

import (
 "context"
 "errors"
 "net/http"
 "strings"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
// ReviewRevisionProvider can lend only the exact admitted session/tab lease and
// must keep it live for the entire transaction. It never opens a requested path.
type ReviewRevisionProvider interface{BeginReviewRevisions(context.Context,Caller,any)(*store.ReviewRevisions,func(),error)}
type ReviewRevisionProviderFunc func(context.Context,Caller,any)(*store.ReviewRevisions,func(),error)
func(f ReviewRevisionProviderFunc)BeginReviewRevisions(ctx context.Context,c Caller,expected any)(*store.ReviewRevisions,func(),error){if f==nil{return nil,nil,errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")};return f(ctx,c,expected)}
func(s *Server)BindReviewRevisions(provider ReviewRevisionProvider)error{
 if provider==nil{return errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")}
 names:=[]string{"db:review-create","db:review-list","db:review-get-latest","db:review-get-full","db:review-next-index","db:revision-create","db:revision-replace-pending","db:revision-list","db:revision-get-pending","db:revision-get-full","db:revision-next-index","db:revision-merge","db:revision-mark-merged","db:revision-mark-discarded"}
 handlers:=make(map[string]Operation,len(names))
 for _,name:=range names{handlers[name]=func(ctx context.Context,caller Caller,args []any)(any,error){
  mutating:=name=="db:review-create"||name=="db:revision-create"||name=="db:revision-replace-pending"||name=="db:revision-merge"||name=="db:revision-mark-merged"||name=="db:revision-mark-discarded"
  fail:=func(err error)(any,error){code:=reviewRevisionCode(err);if !mutating{var httpError *security.HTTPError;if errors.As(err,&httpError){return nil,httpError};return nil,errors.New(code)};text:="Error: "+code;if code=="SOURCE_DRAFT_CHANGED"{text="SourceDraftChangedError: "+code};result:=map[string]any{"success":false,"error":text};if code=="SOURCE_DRAFT_CHANGED"{result["errorCode"]=code};return result,nil}
  arity,pathIndex:=3,1;if name=="db:revision-mark-merged"{arity,pathIndex=4,2};if len(args)!=arity{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}
  expected,err:=draftExpectedContext(args,len(args)-1);if err!=nil{return fail(err)};path,ok:=args[pathIndex].(string);if !ok||path==""{return fail(security.NewHTTPError(http.StatusBadRequest,"PROJECT_CONTEXT_REQUIRED"))};if path!=expected["projectPath"]{return fail(security.NewHTTPError(http.StatusConflict,"PROJECT_CONTEXT_CHANGED"))}
  var input map[string]any;var id,target int64
  structured:=name=="db:review-create"||name=="db:revision-create"||name=="db:revision-replace-pending"||name=="db:revision-merge"
  if structured{input,ok=args[0].(map[string]any);if !ok||input==nil{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};if err:=validateReviewRevisionInput(name,input);err!=nil{return fail(err)}}else{id,ok=draftSafeInteger(args[0],1);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};if name=="db:revision-mark-merged"{target,ok=draftSafeInteger(args[1],1);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}}}
  repository,release,err:=provider.BeginReviewRevisions(ctx,caller,expected);if release!=nil{defer release()};if err!=nil{return fail(err)};if repository==nil||release==nil{return fail(errors.New("REVIEW_REVISION_PROVIDER_UNAVAILABLE"))}
  var result any
  switch name{
  case "db:review-create":result,err=repository.CreateReview(ctx,input)
  case "db:review-list":result,err=repository.ListReviews(ctx,id)
  case "db:review-get-latest":result,err=repository.LatestReview(ctx,id)
  case "db:review-get-full":result,err=repository.FullReview(ctx,id)
  case "db:review-next-index":result,err=repository.NextReviewIndex(ctx,id)
  case "db:revision-create":result,err=repository.CreateRevision(ctx,input)
  case "db:revision-replace-pending":result,err=repository.ReplacePending(ctx,input)
  case "db:revision-list":result,err=repository.ListRevisions(ctx,id,false)
  case "db:revision-get-pending":result,err=repository.ListRevisions(ctx,id,true)
  case "db:revision-get-full":result,err=repository.FullRevision(ctx,id)
  case "db:revision-next-index":result,err=repository.NextRevisionIndex(ctx,id)
  case "db:revision-merge":result,err=repository.MergeRevision(ctx,input)
  case "db:revision-mark-merged":err=repository.MarkMerged(ctx,id,target)
  case "db:revision-mark-discarded":err=repository.MarkDiscarded(ctx,id)
  }
  if err!=nil{return fail(err)};if !mutating{return reviewRevisionWire(result),nil}
  if name=="db:revision-merge"{return map[string]any{"success":true,"receipt":reviewRevisionWire(result)},nil}
  response:=map[string]any{"success":true};if saved,ok:=result.(map[string]any);ok{for _,key:=range []string{"id","reviewIndex","revisionIndex"}{if value,ok:=saved[key];ok{response[key]=value}}};return response,nil
 }}
 s.mu.Lock();defer s.mu.Unlock();for _,name:=range names{if _,ok:=s.registry.Lookup(name);!ok{return errors.New("OPERATION_NOT_EXPOSED")};if _,ok:=s.handlers[name];ok{return errors.New("OPERATION_ALREADY_BOUND")}};for name,handler:=range handlers{s.handlers[name]=handler};return nil
}
func reviewRevisionCode(err error)string{
 var httpError *security.HTTPError;if errors.As(err,&httpError){return httpError.Code};if err==nil{return "REVIEW_REVISION_OPERATION_FAILED"}
 switch err.Error(){case "PROJECT_NOT_OPEN","SOURCE_DRAFT_CHANGED","REVIEW_CYCLE_OWNER_REQUIRED","REVIEW_REVISION_INVALID_BINDING","REVIEW_REVISION_INPUT_INVALID","REVIEW_REVISION_PROVIDER_UNAVAILABLE","DRAFT_NOT_FOUND","REVISION_NOT_FOUND","REVISION_TARGET_MISMATCH","REVISION_ALREADY_MERGED","REVISION_NOT_PENDING","REVISION_TARGET_NOT_EDITABLE","REVISION_TARGET_CHANGED","REVISION_SOURCE_MISSING","REVISION_SOURCE_MISMATCH","REVISION_MERGE_REQUIRED","CONTENT_OWNERSHIP_CONFLICT":return err.Error()};return "REVIEW_REVISION_OPERATION_FAILED"
}
func validateReviewRevisionInput(name string,input map[string]any)error{
 invalid:=func()error{return security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS")}
 // These authorities are not implemented: absence is required, not falsiness.
 for _,key:=range []string{"reviewCycleId","generationRunHandle","authorRecovery"}{if _,exists:=input[key];exists{return security.NewHTTPError(http.StatusConflict,"REVIEW_CYCLE_OWNER_REQUIRED")}}
 if name=="db:revision-merge"{for _,key:=range []string{"revisionId","targetDraftId"}{if _,ok:=draftSafeInteger(input[key],1);!ok{return invalid()}};if _,ok:=draftSafeInteger(input["wordCount"],0);!ok{return invalid()};for _,key:=range []string{"expectedDraftContent","mergedContent"}{if _,ok:=input[key].(string);!ok{return invalid()}};return nil}
 if _,ok:=draftSafeInteger(input["baseDraftId"],1);!ok{return invalid()};if _,ok:=input["content"].(string);!ok{return invalid()}
 if strings.HasPrefix(name,"db:revision-"){kind,ok:=input["revisionType"].(string);if !ok||(kind!="refine"&&kind!="review-fix"){return invalid()};if _,ok:=draftSafeInteger(input["wordCount"],0);!ok{return invalid()};if value,exists:=input["userPrompt"];exists&&value!=nil{if _,ok:=value.(string);!ok{return invalid()}};if value,exists:=input["reviewSourceId"];exists&&value!=nil{if _,ok:=draftSafeInteger(value,1);!ok{return invalid()}}}
 // Missing source is a domain SOURCE_DRAFT_CHANGED, not an HTTP shape failure.
 value,exists:=input["expectedSource"];if !exists||value==nil{return nil};source,ok:=value.(map[string]any);if !ok{return store.ErrSourceDraftChanged};for _,key:=range []string{"id","chapterNumber","version"}{if _,ok:=draftSafeInteger(source[key],1);!ok{return store.ErrSourceDraftChanged}};for _,key:=range []string{"status","content"}{if _,ok:=source[key].(string);!ok{return store.ErrSourceDraftChanged}};return nil
}
func reviewRevisionWire(value any)any{switch value:=value.(type){case []byte:data:=make([]int,len(value));for i,b:=range value{data[i]=int(b)};return map[string]any{"type":"Buffer","data":data};case map[string]any:if value==nil{return nil};result:=make(map[string]any,len(value));for key,item:=range value{result[key]=reviewRevisionWire(item)};return result;case []map[string]any:result:=make([]any,len(value));for i,item:=range value{result[i]=reviewRevisionWire(item)};return result;case []any:result:=make([]any,len(value));for i,item:=range value{result[i]=reviewRevisionWire(item)};return result;default:return value}}
