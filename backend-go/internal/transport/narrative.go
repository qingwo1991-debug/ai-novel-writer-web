package transport

import (
 "context"
 "errors"
 "net/http"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)
type NarrativeProvider interface{BeginNarrative(context.Context,Caller,any)(*store.Narrative,func(),error)}
type NarrativeProviderFunc func(context.Context,Caller,any)(*store.Narrative,func(),error)
func(f NarrativeProviderFunc)BeginNarrative(ctx context.Context,c Caller,expected any)(*store.Narrative,func(),error){if f==nil{return nil,nil,errors.New("NARRATIVE_PROVIDER_REQUIRED")};return f(ctx,c,expected)}
// BindNarrative is an isolated author-led ledger, not AI confirmation authority.
// Frozen outbox evidence is required by the domain even for author confirmation.
func(s *Server)BindNarrative(provider NarrativeProvider)error{
 if provider==nil{return errors.New("NARRATIVE_PROVIDER_REQUIRED")}
 names:=[]string{"db:narrative-thread-list","db:narrative-thread-list-relevant","db:narrative-thread-plan-create","db:narrative-thread-plan-update","db:narrative-thread-plan-delete","db:narrative-thread-event-confirm","db:consistency-exemption-list","db:consistency-exemption-save","db:consistency-exemption-revoke"};handlers:=make(map[string]Operation,len(names))
 for _,name:=range names{handlers[name]=func(ctx context.Context,c Caller,args []any)(any,error){
  caught:=name=="db:consistency-exemption-save"||name=="db:consistency-exemption-revoke"
  fail:=func(err error)(any,error){code:=narrativeCode(err);if caught{return map[string]any{"success":false,"error":"Error: "+code},nil};var public *security.HTTPError;if errors.As(err,&public){return nil,public};return nil,errors.New(code)}
  arity,pathIndex:=3,1;switch name{case "db:narrative-thread-list","db:consistency-exemption-list":arity,pathIndex=2,0;case "db:narrative-thread-plan-update","db:consistency-exemption-save":arity,pathIndex=4,2}
  if len(args)!=arity{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};expected,err:=draftExpectedContext(args,len(args)-1);if err!=nil{return fail(err)};path,ok:=args[pathIndex].(string);if !ok||path==""{return fail(security.NewHTTPError(http.StatusBadRequest,"PROJECT_CONTEXT_REQUIRED"))};if path!=expected["projectPath"]{return fail(security.NewHTTPError(http.StatusConflict,"PROJECT_CONTEXT_CHANGED"))}
  var input map[string]any;var id int64
  switch name{
  case "db:narrative-thread-list-relevant","db:narrative-thread-plan-create","db:narrative-thread-event-confirm":input,ok=args[0].(map[string]any);if !ok||input==nil{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}
  case "db:narrative-thread-plan-update":id,ok=draftSafeInteger(args[0],-9007199254740991);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))};input,ok=args[1].(map[string]any);if !ok||input==nil{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}
  case "db:narrative-thread-plan-delete":id,ok=draftSafeInteger(args[0],-9007199254740991);if !ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}
  case "db:consistency-exemption-save","db:consistency-exemption-revoke":for i:=0;i<pathIndex;i++{if _,ok:=args[i].(string);!ok{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}}
  }
  repository,release,err:=provider.BeginNarrative(ctx,c,expected);if release!=nil{defer release()};if err!=nil{return fail(err)};if repository==nil||release==nil{return fail(errors.New("NARRATIVE_PROVIDER_UNAVAILABLE"))};var result any
  switch name{
  case "db:narrative-thread-list":result,err=repository.List(ctx)
  case "db:narrative-thread-list-relevant":result,err=repository.Relevant(ctx,input)
  case "db:narrative-thread-plan-create":result,err=repository.CreatePlan(ctx,input)
  case "db:narrative-thread-plan-update":result,err=repository.UpdatePlan(ctx,id,input)
  case "db:narrative-thread-plan-delete":err=repository.DeletePlan(ctx,id)
  case "db:narrative-thread-event-confirm":result,err=repository.ConfirmEvent(ctx,input)
  case "db:consistency-exemption-list":result,err=repository.Exemptions(ctx)
  case "db:consistency-exemption-save":err=repository.SaveExemption(ctx,args[0].(string),args[1].(string))
  case "db:consistency-exemption-revoke":err=repository.RevokeExemption(ctx,args[0].(string))
  }
  if err!=nil{return fail(err)};switch name{case "db:narrative-thread-plan-create","db:narrative-thread-plan-update":return map[string]any{"success":true,"plan":reviewRevisionWire(result)},nil;case "db:narrative-thread-event-confirm":return map[string]any{"success":true,"event":reviewRevisionWire(result)},nil;case "db:narrative-thread-plan-delete","db:consistency-exemption-save","db:consistency-exemption-revoke":return map[string]any{"success":true},nil};return reviewRevisionWire(result),nil
 }}
 s.mu.Lock();defer s.mu.Unlock();for _,name:=range names{if _,ok:=s.registry.Lookup(name);!ok{return errors.New("OPERATION_NOT_EXPOSED")};if _,ok:=s.handlers[name];ok{return errors.New("OPERATION_ALREADY_BOUND")}};for name,handler:=range handlers{s.handlers[name]=handler};return nil
}
func narrativeCode(err error)string{
 var public *security.HTTPError;if errors.As(err,&public){return public.Code};if err==nil{return "NARRATIVE_OPERATION_FAILED"}
 switch err.Error(){case "PROJECT_NOT_OPEN","NARRATIVE_INPUT_INVALID","NARRATIVE_PROVIDER_UNAVAILABLE","NARRATIVE_THREAD_PLAN_INVALID","NARRATIVE_THREAD_RANGE_INVALID","NARRATIVE_THREAD_PLAN_NOT_FOUND","NARRATIVE_THREAD_EVENT_INVALID","NARRATIVE_THREAD_EVENT_SOURCE_NOT_FINALIZED","NARRATIVE_THREAD_EVIDENCE_NOT_IN_SOURCE","CONSISTENCY_EXEMPTION_KEY_INVALID","CONSISTENCY_EXEMPTION_REASON_INVALID":return err.Error()};return "NARRATIVE_OPERATION_FAILED"
}
