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

// publicErrorCode is the symbolic code writePublicError would put on the wire
// for err, computed without writing a response. Empty means no public code.
func publicErrorCode(err error)string {
 if err==nil{return ""}
 var public *security.HTTPError
 if errors.As(err,&public)&&public!=nil{return public.Code}
 text:=err.Error();if len(text)<2||text[0]<'A'||text[0]>'Z'{return ""}
 for i:=1;i<len(text);i++{b:=text[i];if (b<'A'||b>'Z')&&(b<'0'||b>'9')&&b!='_'{return ""}}
 return text
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
