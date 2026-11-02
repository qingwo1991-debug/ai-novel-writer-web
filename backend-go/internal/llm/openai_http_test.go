package llm

import (
 "context"
 "encoding/json"
 "io"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync/atomic"
 "testing"
 "time"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)
func TestOpenAIRealHTTPWithNativeEgressAndCancellation(t *testing.T){
 var calls atomic.Int64;observed:=make(chan struct{},1)
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  calls.Add(1);if r.URL.Path!="/v1/chat/completions"||r.Header.Get("Authorization")!="Bearer public-synthetic"{t.Error("wrong request routing")}
  var body map[string]any;if json.NewDecoder(r.Body).Decode(&body)!=nil{t.Error("invalid JSON")};if _,ok:=body["temperature"];ok{t.Error("implicit temperature")}
  w.Header().Set("Content-Type","text/event-stream");io.WriteString(w,"data: {\"choices\":[{\"delta\":{\"content\":\"safe <thi\"}}]}\n\n");w.(http.Flusher).Flush()
  if body["model"]=="cancel"{observed<-struct{}{};<-r.Context().Done();return}
  io.WriteString(w,"data: {\"choices\":[{\"delta\":{\"content\":\"nk>private</think> visible\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\n\n")
 }));defer server.Close()
 client,err:=egress.New([]string{server.URL});if err!=nil{t.Fatal(err)};defer client.Close();provider:=OpenAI{Dispatch:client.Do}
 model:=Model{Provider:"openai",BaseURL:server.URL+"/v1",ModelName:"normal",MaxTokens:128,APIKey:"public-synthetic"}
 text:="";done:=0;provider.Stream(context.Background(),model,[]Message{{"user","synthetic"}},Options{VisibleOnly:true},Callbacks{OnChunk:func(s string){text+=s},OnDone:func(s string,u *Usage,finish string){done++;if s!="safe  visible"||finish!="stop"{t.Error("semantic completion",s,finish)}},OnError:func(code,content string,u *Usage){t.Error(code)}})
 if done!=1||text!="safe  visible"{t.Fatal("reasoning leak/completion",text,done)}
 model.ModelName="cancel";ctx,cancel:=context.WithCancel(context.Background());finished:=make(chan struct{});code,candidate:="",""
 go func(){provider.Stream(ctx,model,nil,Options{VisibleOnly:true},Callbacks{OnError:func(c,s string,u *Usage){code,candidate=c,s},OnDone:func(string,*Usage,string){t.Error("cancel committed")}});close(finished)}()
 select{case <-observed:cancel();case <-time.After(5*time.Second):cancel();t.Fatal("server did not stream")}
 select{case <-finished:case <-time.After(5*time.Second):t.Fatal("cancel did not stop native request")}
 if code!="LLM_CANCELLED"||strings.Contains(candidate,"private"){t.Fatal(code,candidate)}
 model.BaseURL="https://forbidden.invalid/v1";provider.Stream(context.Background(),model,nil,Options{VisibleOnly:true},Callbacks{OnError:func(c,s string,u *Usage){if c==""{t.Error("policy denial missing")}}});if calls.Load()!=2{t.Fatal("unexpected network dispatch")}
}
func TestProviderSSELimitAndFinalFraming(t *testing.T){
 for _,wire:=range []string{"data:x\r\rdata:y\r\r","data:x\r\n\r\ndata:y","data:x\n\ndata:y\n\n"}{var events []string;if err:=readProviderSSE(context.Background(),strings.NewReader(wire),func(s string)bool{events=append(events,s);return true});err!=nil{t.Fatal(err)};if strings.Join(events,",")!="x,y"{t.Fatal(events)}}
 if err:=readProviderSSE(context.Background(),strings.NewReader("data:"+strings.Repeat("x",maxSSEEventBytes)),func(string)bool{t.Error("oversize event emitted");return true});err!=errSSELimit{t.Fatal(err)}
}
