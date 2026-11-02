package transport

import (
 "context"
 "errors"
 "net/http"
 "unicode/utf8"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// KnowledgeProvider lends an admitted copied knowledge view and finalized-document
// chapter facts from the SAME captured project session. The supplied frozen context
// must match both cookie session and tab; its path is not a filesystem capability.
// A nonnil release holds that session stable through the entire read, including
// annotation. The transport releases it exactly once even when Begin returns an
// error. Providers must not open request paths, infer model state or perform writes.
// No original author-project or durable project:open authority is granted here.
type KnowledgeProvider interface {
 BeginKnowledge(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error)
}
type KnowledgeProviderFunc func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error)
func (f KnowledgeProviderFunc) BeginKnowledge(ctx context.Context, caller Caller, expected any) (*knowledgequery.View, map[string]int64, func(), error) {
 if f==nil{return nil,nil,nil,errors.New("KNOWLEDGE_PROVIDER_REQUIRED")}
 return f(ctx,caller,expected)
}

// BindKnowledge installs only four read operations atomically. Query results keep
// their original shapes; only list-documents adds finalizedChapter when an exact
// document identity exists in the same-session finalized map. This is not a search,
// embedding, import, rebuild, model/provider dispatch, or writer binding.
func (s *Server) BindKnowledge(provider KnowledgeProvider) error {
 if provider==nil{return errors.New("KNOWLEDGE_PROVIDER_REQUIRED")}
 names:=[]string{"kb:list-documents","kb:stats","kb:get-vectorless-count","kb:read-document-copy"}
 handlers:=make(map[string]Operation,len(names))
 for _,name:=range names {
  handlers[name]=func(ctx context.Context,caller Caller,args []any)(any,error){
   arity,pathIndex:=2,0;if name=="kb:read-document-copy"{arity,pathIndex=3,1}
   fail:=func(err error)(any,error){return nil,knowledgeReadError(err)}
   if len(args)!=arity{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}
   expected,err:=draftExpectedContext(args,len(args)-1);if err!=nil{return fail(err)}
   path,ok:=args[pathIndex].(string);if !ok||path==""||!utf8.ValidString(path)||int64(len(path))>security.DefaultBodyLimit{return fail(security.NewHTTPError(http.StatusBadRequest,"PROJECT_CONTEXT_REQUIRED"))}
   if path!=expected["projectPath"]{return fail(security.NewHTTPError(http.StatusConflict,"PROJECT_CONTEXT_CHANGED"))}
   var docID string
   if name=="kb:read-document-copy"{docID,ok=args[0].(string);if !ok||docID==""||!utf8.ValidString(docID)||int64(len(docID))>security.DefaultBodyLimit{return fail(security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS"))}}
   if ctx==nil{return fail(errors.New("KNOWLEDGE_READ_FAILED"))};if err:=ctx.Err();err!=nil{return fail(err)}
   view,finalized,release,err:=provider.BeginKnowledge(ctx,caller,expected)
   if release!=nil{defer release()}
   if err!=nil{return fail(err)}
   if view==nil||release==nil{return fail(errors.New("KNOWLEDGE_PROVIDER_UNAVAILABLE"))}
   switch name {
   case "kb:list-documents":
    documents,err:=view.Documents(ctx);if err!=nil{return fail(err)}
    // Copy every row so annotation can never mutate a cached/private view row.
    result:=make([]map[string]any,0,len(documents));for _,document:=range documents{row:=make(map[string]any,len(document)+1);for key,value:=range document{row[key]=value};if id,ok:=document["id"].(string);ok{if chapter,exists:=finalized[id];exists{row["finalizedChapter"]=chapter}};result=append(result,row)}
    return result,nil
   case "kb:stats":result,err:=view.Stats(ctx);if err!=nil{return fail(err)};return result,nil
   case "kb:get-vectorless-count":result,err:=view.VectorlessCount(ctx);if err!=nil{return fail(err)};return result,nil
   default:result,err:=view.ReadCopy(ctx,docID);if err!=nil{return fail(err)};return result,nil
   }
  }
 }
 s.mu.Lock();defer s.mu.Unlock()
 for _,name:=range names{if _,ok:=s.registry.Lookup(name);!ok{return errors.New("OPERATION_NOT_EXPOSED")};if _,ok:=s.handlers[name];ok{return errors.New("OPERATION_ALREADY_BOUND")}}
 for name,handler:=range handlers{s.handlers[name]=handler}
 return nil
}

// A repository, filesystem or native-library error may contain private content.
// Keep already-public HTTP errors; otherwise expose only this finite symbol list,
// never a raw error string or arbitrary all-uppercase error supplied by a provider.
func knowledgeReadError(err error) error {
 var public *security.HTTPError;if errors.As(err,&public){if public!=nil{return public};return errors.New("KNOWLEDGE_READ_FAILED")}
 if errors.Is(err,context.Canceled){return errors.New("REQUEST_CANCELED")}
 if errors.Is(err,context.DeadlineExceeded){return errors.New("REQUEST_DEADLINE_EXCEEDED")}
 if err!=nil{switch err.Error(){
 case "PROJECT_SESSION_NOT_OWNED","PROJECT_SESSION_CONTEXT_REQUIRED","PROJECT_SESSION_MANAGER_CLOSED","PROJECT_SESSION_STALE","PROJECT_SESSION_ROOT_MISMATCH","PROJECT_NOT_OPEN","KNOWLEDGE_PROVIDER_UNAVAILABLE","KNOWLEDGE_QUERY_INPUT_INVALID","KNOWLEDGE_QUERY_DOMAIN_INVALID","KNOWLEDGE_QUERY_COPIES_INVALID":return errors.New(err.Error())
 }}
 return errors.New("KNOWLEDGE_READ_FAILED")
}
