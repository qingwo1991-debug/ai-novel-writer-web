package transport

import (
 "errors"
 "net/http"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// RegisterSecret must precede admitting or using a saved/new private credential,
// including before the first provider request/event. Capacity failure denies the
// operation rather than evicting active credentials. This is literal redaction,
// not arbitrary secret discovery and not authorization for raw config DTOs.
func(s *Server) RegisterSecret(value string)(bool,error) {
 return s.public.RegisterSecret(value)
}

func(s *Server) writePublicError(w http.ResponseWriter,err error)error {
 status:=http.StatusInternalServerError
 code:="REQUEST_FAILED"
 message:="服务暂时无法完成此操作，请重试或查看服务器日志。"
 var public *security.HTTPError
 if errors.As(err,&public)&&public!=nil {status,code,message=public.Status,public.Code,public.Error()} else if err!=nil {
  text:=err.Error();symbolic:=len(text)>=2&&text[0]>='A'&&text[0]<='Z'
  for i:=1;i<len(text)&&symbolic;i++ {b:=text[i];symbolic=(b>='A'&&b<='Z')||(b>='0'&&b<='9')||b=='_'}
  if symbolic {code=text}
 }
 return security.WriteJSON(w,status,s.public.Sanitize(map[string]any{"error":map[string]any{"code":code,"message":message}}))
}
