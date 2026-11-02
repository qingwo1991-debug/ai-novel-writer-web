package mcp

import (
 "context"
 "strings"
 "testing"
 "time"
)

func TestStdioMalformedAndTruncatedFramesSettlePendingSafely(t *testing.T){
 cases:=[]struct{name,raw,code string}{
  {"malformed","PRIVATE_SERVER_DEBUG_NOT_FOR_BROWSER\n","MCP_STDOUT_INVALID"},
  {"utf8",string([]byte{255,'\n'}),"MCP_STDOUT_INVALID"},
  {"truncated",`{"jsonrpc":"2.0","id":1,"result":{}}`,"MCP_STDOUT_INVALID"},
  {"oversized",strings.Repeat("x",MaxFrameBytes+1),"MCP_STDOUT_LIMIT_EXCEEDED"},
 }
 for _,entry:=range cases{t.Run(entry.name,func(t *testing.T){
  c:=newConnection(serverConfig{ID:"fixture"},Options{RequestTimeout:time.Second});defer c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")
  pending:=make(chan response,1);c.pending[1]=pending
  c.readStdio(strings.NewReader(entry.raw))
  reply:=<-pending;if reply.err==nil||reply.err.Error()!=entry.code{t.Fatalf("safe error: %v want %s",reply.err,entry.code)}
  if c.status!="error"||len(c.pending)!=0{t.Fatal("malformed transport not terminal")}
 })}
}
func TestRemoteErrorTextIsNotExposed(t *testing.T){
 c:=newConnection(serverConfig{ID:"fixture"},Options{RequestTimeout:time.Second});defer c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")
 result:=make(chan error,1);go func(){_,err:=c.request(context.Background(),"tools/call",nil);result<-err}()
 message:=<-c.queue
 c.mu.Lock();c.queuedBytes-=len(message.raw);c.mu.Unlock()
 if err:=c.receive([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"SECRET_SERVER_ENV_AND_AUTHOR_PATH"}}`));err!=nil{t.Fatal(err)}
 if err:=<-result;err==nil||err.Error()!="MCP_REQUEST_FAILED"{t.Fatalf("unsafe remote error: %v",err)}
}
func TestUnsolicitedServerRequestCannotExecuteBusiness(t *testing.T){
 c:=newConnection(serverConfig{ID:"fixture"},Options{RequestTimeout:time.Second});defer c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")
 if err:=c.receive([]byte(`{"jsonrpc":"2.0","id":"server-initiated","method":"executeArbitraryCommand","params":{"command":"not-executed"}}`));err!=nil{t.Fatal(err)}
 reply:=<-c.queue
 if !strings.Contains(string(reply.raw),`"code":-32601`)||strings.Contains(string(reply.raw),"not-executed"){t.Fatalf("unsafe callback response: %s",reply.raw)}
}
