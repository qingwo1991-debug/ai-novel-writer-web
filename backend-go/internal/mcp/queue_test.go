package mcp

import (
 "context"
 "strings"
 "testing"
 "time"
)

func TestBlockedWriterDropsSecondCanceledRequest(t *testing.T){
 for _,disable:=range []bool{false,true}{name:="cancel";if disable{name="disable"};t.Run(name,func(t *testing.T){
  c:=newConnection(serverConfig{ID:"fixture"},Options{RequestTimeout:time.Second});defer c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")
  firstStarted:=make(chan struct{});releaseFirst:=make(chan struct{});finished:=make(chan struct{});sent:=make(chan string,4)
  c.mu.Lock();c.pending[1]=make(chan response,1);if err:=c.enqueueLocked(queuedMessage{raw:[]byte("first-in-flight"),id:1,ctx:context.Background()});err!=nil{t.Fatal(err)};c.mu.Unlock()
  go func(){c.writer(func(_ context.Context,raw []byte)error{
   sent<-string(raw)
   if string(raw)=="first-in-flight"{close(firstStarted);<-releaseFirst;return nil}
   c.stop("disconnected","MCP_SERVER_NOT_CONNECTED");return nil
  });close(finished)}()
  <-firstStarted
  ctx,cancel:=context.WithCancel(context.Background());defer cancel()
  c.mu.Lock();c.pending[2]=make(chan response,1);if err:=c.enqueueLocked(queuedMessage{raw:[]byte("second-must-not-send"),id:2,ctx:ctx});err!=nil{t.Fatal(err)};c.mu.Unlock()
  if disable{c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")}else{cancel();if err:=c.notify("sentinel",nil);err!=nil{t.Fatal(err)}}
  close(releaseFirst)
  select{case <-finished:case <-time.After(time.Second):t.Fatal("writer failed to finish")}
  close(sent);for payload:=range sent{if strings.Contains(payload,"second-must-not-send"){t.Fatal("queued second request sent after revocation")}}
 })}
}

func TestRequestTimeoutCompletesPendingAndSkipsQueuedSend(t *testing.T){
 c:=newConnection(serverConfig{ID:"fixture"},Options{RequestTimeout:time.Millisecond});defer c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")
 if _,err:=c.request(context.Background(),"tools/call",map[string]any{});err==nil||err.Error()!="MCP_REQUEST_TIMEOUT"{t.Fatalf("timeout: %v",err)}
 c.mu.Lock();if len(c.pending)!=0{t.Fatal("timeout retained pending request")};c.mu.Unlock()
 done:=make(chan struct{});writes:=make(chan string,2)
 go func(){c.writer(func(_ context.Context,raw []byte)error{writes<-string(raw);c.stop("disconnected","MCP_SERVER_NOT_CONNECTED");return nil});close(done)}()
 if err:=c.notify("sentinel",nil);err!=nil{t.Fatal(err)}
 select{case <-done:case <-time.After(time.Second):t.Fatal("writer stuck")}
 close(writes);for payload:=range writes{if strings.Contains(payload,"tools/call"){t.Fatal("timed out request sent late")}}
}
