package llm

import (
 "context"
 "errors"
 "io"
 "net/http"
 "strings"
 "testing"
)
type finalCancelReader struct{data []byte;cancel context.CancelFunc;closed bool}
func(r *finalCancelReader)Read(p []byte)(int,error){if len(r.data)==0{return 0,io.EOF};n:=copy(p,r.data);r.data=r.data[n:];if len(r.data)==0{r.cancel()};return n,nil}
func(r *finalCancelReader)Close()error{r.closed=true;return nil}
func TestFinalReadCancellationNeverCompletesAndCloses(t *testing.T){
 model:=Model{BaseURL:"https://example.invalid/v1",Provider:"openai",ModelName:"synthetic",MaxTokens:128}
 for _,stream:=range []bool{false,true}{ctx,cancel:=context.WithCancel(context.Background());defer cancel();wire:=`{"choices":[{"message":{"content":"safe"},"finish_reason":"stop"}]}`;if stream{wire="data: [DONE]\n\n"}
  reader:=&finalCancelReader{data:[]byte(wire),cancel:cancel};provider:=OpenAI{Dispatch:func(*http.Request)(*http.Response,error){return &http.Response{StatusCode:200,Body:reader},nil}}
  if !stream{r:=provider.Generate(ctx,model,nil,Options{});if r.Success||r.Error!="LLM_CANCELLED"{t.Fatal(r)}}else{failed:=0;provider.Stream(ctx,model,nil,Options{VisibleOnly:true},Callbacks{OnDone:func(string,*Usage,string){t.Error("cancelled response completed")},OnError:func(code,content string,u *Usage){failed++;if code!="LLM_CANCELLED"{t.Error(code)}}});if failed!=1{t.Fatal("missing terminal error")}}
  if !reader.closed{t.Fatal("body not closed")}
 }
 reader:=&finalCancelReader{cancel:func(){}};provider:=OpenAI{Dispatch:func(*http.Request)(*http.Response,error){return &http.Response{Body:reader},errors.New("synthetic error with private URL")}};r:=provider.Generate(context.Background(),model,nil,Options{});if !reader.closed||strings.Contains(r.Error,"private"){t.Fatal("response/error leak")}
}
type repeatedHeartbeat struct{remaining int}
func(r *repeatedHeartbeat)Read(p []byte)(int,error){if r.remaining<=0{return 0,io.EOF};n:=len(p);if n>r.remaining{n=r.remaining};for i:=0;i<n;i++{if i%2==0{p[i]=':'}else{p[i]='\n'}};r.remaining-=n;return n,nil}
func TestSSEAggregateHeartbeatBytesBounded(t *testing.T){
 reader:=&repeatedHeartbeat{remaining:maxProviderBytes+32768};if err:=readProviderSSE(context.Background(),reader,func(string)bool{t.Error("comment became data");return true});!errors.Is(err,errSSELimit){t.Fatal("unbounded heartbeat",err)}
}
