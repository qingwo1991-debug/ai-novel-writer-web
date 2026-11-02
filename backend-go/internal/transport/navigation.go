package transport

import "context"

const officialHomepage="https://github.com/EthanYoQ/AI-Novel-Writer"
var providerResources=map[string]string{
 "siliconflow-invite":"https://cloud.siliconflow.cn/i/klFgdwZa",
 "siliconflow-console":"https://cloud.siliconflow.cn/",
 "siliconflow-docs":"https://docs.siliconflow.cn/",
}

// BindNavigation ports the two fixed external-link intents. The only side effect
// is a private event on the authenticated originating session+tab hub. No URL is
// supplied by a client, no network request is made, and no process is launched.
func(s *Server)BindNavigation()error{
 if err:=s.Bind("official-homepage:open",func(ctx context.Context,caller Caller,args []any)(any,error){
  if err:=ctx.Err();err!=nil{return nil,err}
  if err:=s.Publish(caller,"browser:open-external",[]any{map[string]any{"url":officialHomepage}});err!=nil{
   return map[string]any{"success":false,"error":"Unable to open the official homepage."},nil
  }
  return map[string]any{"success":true},nil
 });err!=nil{return err}
 return s.Bind("model-provider-resource:open",func(ctx context.Context,caller Caller,args []any)(any,error){
  var id string;if len(args)>0{id,_=args[0].(string)}
  url,ok:=providerResources[id];if !ok{return map[string]any{"success":false,"error":"Unsupported model provider resource."},nil}
  if err:=ctx.Err();err!=nil{return nil,err}
  if err:=s.Publish(caller,"browser:open-external",[]any{map[string]any{"url":url}});err!=nil{
   return map[string]any{"success":false,"error":"Unable to open the model provider resource."},nil
  }
  return map[string]any{"success":true},nil
 })
}
