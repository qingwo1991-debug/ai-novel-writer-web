package transport

import (
 "context"
 "errors"
 "net/http"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindPromptSkills exposes prompt CRUD and installed skill reads/removal only.
// Remote/local installation remains UNBOUND until upload/egress/inspection owner
// composition is qualified. The root is captured by a trusted backend provider.
func(s *Server) BindPromptSkills(provider *promptskills.Service,startup *globalconfig.Startup)error {
 if provider==nil||startup==nil{return errors.New("PROMPT_SKILLS_PROVIDER_REQUIRED")}
 handlers:=map[string]Operation{}
 for _,name:=range []string{"prompt:load-global","prompt:save-global","prompt:delete-global","skills:list-user","skills:uninstall-user"}{
  operation:=name
  handlers[operation]=func(ctx context.Context,caller Caller,args []any)(any,error){
   if startup.State(startupCaller(caller),true).State!="ready"{return nil,security.NewHTTPError(http.StatusConflict,"GLOBAL_DATA_NOT_READY")}
   if err:=ctx.Err();err!=nil{return nil,errors.New("PROMPT_SKILLS_CANCELLED")}
   invalid:=func()(any,error){return nil,security.NewHTTPError(http.StatusBadRequest,"INVALID_ARGUMENTS")}
   failure:=func(code string)(any,error){return map[string]any{"success":false,"error":"Error: "+code},nil}
   switch operation {
   case "prompt:load-global":
    if len(args)!=0{return invalid()};value,err:=provider.LoadPrompts();if err!=nil{return nil,errors.New("PROMPT_LOAD_FAILED")};return value,nil
   case "prompt:save-global":
    if len(args)!=1{return invalid()};template,ok:=args[0].(map[string]any);if !ok{return invalid()}
    if err:=provider.SavePrompt(template);err!=nil{return failure("PROMPT_SAVE_FAILED")};return map[string]any{"success":true},nil
   case "prompt:delete-global":
    if len(args)!=2{return invalid()};key,kok:=args[0].(string);language,lok:=args[1].(string);if !kok||!lok{return invalid()}
    if err:=provider.DeletePrompt(key,language);err!=nil{return failure("PROMPT_DELETE_FAILED")};return map[string]any{"success":true},nil
   case "skills:list-user":
    if len(args)!=0{return invalid()};skills,err:=provider.ListUser();if err!=nil{return nil,errors.New("SKILLS_LIST_FAILED")}
    return promptSkillsListWire(skills),nil
   case "skills:uninstall-user":
    if len(args)!=1{return invalid()};name,ok:=args[0].(string);if !ok{return invalid()}
    if err:=provider.UninstallUser(name);err!=nil{return failure("SKILLS_UNINSTALL_FAILED")};return map[string]any{"success":true},nil
   }
   return nil,errors.New("OPERATION_NOT_EXPOSED")
  }
 }
 return s.bindOperations(handlers)
}
