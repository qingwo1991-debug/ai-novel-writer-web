// Package transport implements the existing browser HTTP envelope in Go.
// A known operation without a Go handler fails closed with 501: no Node fallback.
package transport

import (
 "context"
 "errors"
 "fmt"
 "net/http"
 "regexp"
 "strconv"
 "strings"
 "sync"
 "time"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/publicresponse"
 novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

type Caller struct { Principal string; SessionID string; ClientID string }
type Operation func(context.Context,Caller,[]any)(any,error)
type client struct {hub *novelruntime.EventHub; inflight int; pinned int; touched time.Time}
type Server struct {
 sessions *security.Sessions
 origins security.Origins
 registry *policy.Registry
 public *publicresponse.Sanitizer
 generation string
 mu sync.Mutex
 clients map[string]*client
 handlers map[string]Operation
 now func()time.Time
}
func(s *Server)clock()time.Time{if s.now!=nil{return s.now()};return time.Now()}
func New(registry *policy.Registry,origins security.Origins,generation string)*Server{
 return &Server{sessions:security.NewSessions(),origins:origins,registry:registry,public:publicresponse.New(),generation:generation,clients:make(map[string]*client),handlers:make(map[string]Operation)}
}
// Bind registers only inventoried operations. The domain handler must additionally
// enforce project/capability ownership and domain admission; routing is not auth.
// Call before serving; dynamic handler replacement is deliberately unsupported.
func(s *Server)Bind(operation string,handler Operation)error{
 if _,ok:=s.registry.Lookup(operation);!ok{return errors.New("OPERATION_NOT_EXPOSED")}
 if handler==nil{return errors.New("OPERATION_HANDLER_REQUIRED")}
 s.mu.Lock();defer s.mu.Unlock();if _,ok:=s.handlers[operation];ok{return errors.New("OPERATION_ALREADY_BOUND")};s.handlers[operation]=handler;return nil
}
var clientPattern=regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`)
func(s *Server)getClient(sessionID,token string)(*client,error){
 if !clientPattern.MatchString(token){return nil,security.NewHTTPError(400,"CLIENT_ID_REQUIRED")}
 s.mu.Lock();defer s.mu.Unlock();now:=s.clock();prefix:=sessionID+":";count:=0
 for key,entry:=range s.clients{
  if entry.pinned==0&&entry.inflight==0&&entry.hub.Connections()==0&&now.Sub(entry.touched)>2*time.Minute{entry.hub.Close();delete(s.clients,key);continue}
  if strings.HasPrefix(key,prefix){count++}
 }
 key:=prefix+token
 if entry:=s.clients[key];entry!=nil{entry.touched=now;entry.pinned++;return entry,nil}
 if count>=32{return nil,security.NewHTTPError(429,"TAB_LIMIT")}
 entry:=&client{hub:novelruntime.NewEventHub(),touched:now,pinned:1};s.clients[key]=entry;return entry,nil
}
func(s *Server)unpin(current *client){s.mu.Lock();defer s.mu.Unlock();current.pinned--;current.touched=s.clock()}
func(s *Server)ServeHTTP(w http.ResponseWriter,r *http.Request){
 if err:=s.serve(w,r);err!=nil{_ = s.writePublicError(w,err)}
}
func(s *Server)serve(w http.ResponseWriter,r *http.Request)error{
 if r.URL.Path=="/healthz"{return security.WriteJSON(w,200,map[string]any{"ok":true,"runtime":"go","electron":false,"migrationComplete":false})}
 principal,err:=security.Authenticate(r);if err!=nil{return err}
 session,err:=s.sessions.Get(r,w,principal);if err!=nil{return err}
 if r.URL.Path=="/api/bootstrap"&&r.Method==http.MethodGet{return security.WriteJSON(w,200,map[string]any{"csrfToken":session.CSRFToken(),"runtime":"web","generation":s.generation})}
 token:=r.Header.Get("X-Client-Id")
 if r.URL.Path=="/api/events"&&r.Method==http.MethodGet{
  if err:=security.CheckEventOrigin(r,s.origins);err!=nil{return err};token=r.URL.Query().Get("client")
  current,err:=s.getClient(session.ID(),token);if err!=nil{return err};defer s.unpin(current);return s.events(w,r,current)
 }
 if !strings.HasPrefix(r.URL.Path,"/api/"){return security.NewHTTPError(404,"NOT_FOUND")}
 current,err:=s.getClient(session.ID(),token);if err!=nil{return err};defer s.unpin(current)
 if r.Method!=http.MethodGet{if err:=s.sessions.Verify(r,session,s.origins);err!=nil{return err}}
 if strings.HasPrefix(r.URL.Path,"/api/operations/")&&r.Method==http.MethodPost{
  s.mu.Lock();if current.inflight>=32{s.mu.Unlock();return security.NewHTTPError(429,"REQUEST_LIMIT")};current.inflight++;s.mu.Unlock()
  defer func(){s.mu.Lock();current.inflight--;current.touched=s.clock();s.mu.Unlock()}()
  input,err:=security.JSONBody(r,0);if err!=nil{return err};object,ok:=input.(map[string]any);if !ok{return security.NewHTTPError(400,"INVALID_ARGUMENTS")}
  if err:=security.ValidateArguments(object["args"]);err!=nil{return err};args:=object["args"].([]any)
  operation:=strings.TrimPrefix(r.URL.Path,"/api/operations/")
  if _,ok:=s.registry.Lookup(operation);!ok{return security.NewHTTPError(404,"OPERATION_NOT_EXPOSED")}
  s.mu.Lock();handler:=s.handlers[operation]
  if handler==nil{s.mu.Unlock();return security.NewHTTPError(501,"GO_OPERATION_NOT_MIGRATED")}
  s.mu.Unlock()
  result,err:=handler(r.Context(),Caller{Principal:principal,SessionID:session.ID(),ClientID:token},args);if err!=nil{return err}
  return security.WriteJSON(w,200,s.public.Sanitize(map[string]any{"result":result}))
 }
 // Release/transfer/dialog semantics require actual domain ownership and are
 // deliberately not stubbed as success in this initial migration foundation.
 if r.URL.Path=="/api/release"||r.URL.Path=="/api/library"||strings.HasPrefix(r.URL.Path,"/api/dialogs/")||r.URL.Path=="/api/uploads"||r.URL.Path=="/api/upload-folders"||strings.HasPrefix(r.URL.Path,"/api/downloads/"){return security.NewHTTPError(501,"GO_ROUTE_NOT_MIGRATED")}
 return security.NewHTTPError(404,"API_ROUTE_NOT_FOUND")
}
func(s *Server)events(w http.ResponseWriter,r *http.Request,current *client)error{
 flusher,ok:=w.(http.Flusher);if !ok{return security.NewHTTPError(500,"SSE_NOT_SUPPORTED")}
 last,_:=strconv.ParseUint(r.Header.Get("Last-Event-ID"),10,64)
 history,queue,cancel,err:=current.hub.Subscribe(last);if err!=nil{if errors.Is(err,novelruntime.ErrEventLimit){return security.NewHTTPError(429,"EVENT_CONNECTION_LIMIT")};return err};defer cancel()
 w.Header().Set("Content-Type","text/event-stream");w.Header().Set("Cache-Control","no-store");w.Header().Set("X-Accel-Buffering","no")
 controller:=http.NewResponseController(w)
 write:=func(bytes []byte)error{_ = controller.SetWriteDeadline(time.Now().Add(15*time.Second));_,err:=w.Write(bytes);if err==nil{flusher.Flush()};return err}
 if err:=write([]byte(": connected\n\n"));err!=nil{return nil}
 for _,event:=range history{if err:=write(event.Wire());err!=nil{return nil}}
 heartbeat:=time.NewTicker(15*time.Second);defer heartbeat.Stop()
 for{select{
 case<-r.Context().Done():return nil
 case event,ok:=<-queue:if !ok{return nil};err:=write(event.Wire());event.Done();if err!=nil{return nil}
 case<-heartbeat.C:s.mu.Lock();current.touched=s.clock();s.mu.Unlock();if err:=write([]byte(": heartbeat\n\n"));err!=nil{return nil}
 }}
}
// Publish requires a previously established caller tab. Sanitize before storing
// history so live events and later replay share the same credential boundary.
// Domains must still project safe DTOs, not raw private configuration objects.
func(s *Server)Publish(caller Caller,channel string,args []any)error{
 s.mu.Lock();entry:=s.clients[caller.SessionID+":"+caller.ClientID];s.mu.Unlock();if entry==nil{return fmt.Errorf("CLIENT_NOT_FOUND")};if args==nil{args=[]any{}};clean,ok:=s.public.Sanitize(args).([]any);if !ok{return errors.New("PUBLIC_EVENT_INVALID")};name,ok:=s.public.Sanitize(channel).(string);if !ok{return errors.New("PUBLIC_EVENT_INVALID")};return entry.hub.Publish(name,clean)
}
func(s *Server)Close(){s.mu.Lock();defer s.mu.Unlock();for key,entry:=range s.clients{entry.hub.Close();delete(s.clients,key)}}
