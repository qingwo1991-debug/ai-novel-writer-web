package mcp

import (
 "bufio"
 "bytes"
 "context"
 "errors"
 "io"
 "mime"
 "net/http"
 "net/url"
 "os"
 "os/exec"
 "sort"
 "strings"
 "time"
 "unicode/utf8"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)

// childEnvironment intentionally does not inherit HOME, app secrets, proxy
// credentials, Node/loader injection or worker configuration. An administrator
// may intentionally provide any of those for their independently trusted tool.
func childEnvironment(runtimeDir string,overrides map[string]string)[]string{
 values:=make(map[string]string)
 for _,key:=range []string{"PATH","LANG","LC_ALL","LC_CTYPE","TZ","SystemRoot","WINDIR","PATHEXT","COMSPEC"}{if val,ok:=os.LookupEnv(key);ok{values[key]=val}}
 values["HOME"]=runtimeDir
 for key,val:=range overrides{values[key]=val}
 keys:=make([]string,0,len(values));for key:=range values{keys=append(keys,key)};sort.Strings(keys)
 out:=make([]string,0,len(keys));for _,key:=range keys{out=append(out,key+"="+values[key])};return out
}
func(c *connection)startStdio()error{
 if c.config.Command==""||c.options.RuntimeDir==""{return errors.New("MCP_CONNECT_FAILED")}
 if err:=os.MkdirAll(c.options.RuntimeDir,0700);err!=nil{return errors.New("MCP_CONNECT_FAILED")}
 // No shell, no caller command, no command discovery from raw RPC. exec is
 // exclusively for this explicitly trusted administrator-selected external tool.
 cmd:=exec.Command(c.config.Command,c.config.Args...)
 cmd.Dir=c.options.RuntimeDir;cmd.Env=childEnvironment(c.options.RuntimeDir,c.config.Env);cmd.WaitDelay=2*time.Second
 stdin,err:=cmd.StdinPipe();if err!=nil{return errors.New("MCP_CONNECT_FAILED")}
 stdout,err:=cmd.StdoutPipe();if err!=nil{_ = stdin.Close();return errors.New("MCP_CONNECT_FAILED")}
 // Discard stderr directly; never retain or broadcast unbounded diagnostics.
 cmd.Stderr=io.Discard
 c.mu.Lock()
 if !c.alive(){c.mu.Unlock();_ = stdin.Close();_ = stdout.Close();return errors.New("MCP_SERVER_NOT_CONNECTED")}
 if err=cmd.Start();err!=nil{c.mu.Unlock();_ = stdin.Close();_ = stdout.Close();return errors.New("MCP_CONNECT_FAILED")}
 c.closeTransport=func(){_ = stdin.Close();_ = stdout.Close();_ = cmd.Process.Kill()};c.mu.Unlock()
 go c.readStdio(stdout)
 go func(){_ = cmd.Wait();c.stop("disconnected","MCP_SERVER_NOT_CONNECTED")}()
 go c.writer(func(ctx context.Context,raw []byte)error{
  if ctx.Err()!=nil||c.ctx.Err()!=nil{return errors.New("MCP_REQUEST_CANCELED")}
  // os/exec stdin is a native pipe. Bound a tool that stops reading even when
  // no response arrives; never leave the single writer goroutine stuck forever.
  if pipe,ok:=stdin.(interface{SetWriteDeadline(time.Time)error});ok{deadline:=time.Now().Add(c.options.RequestTimeout);if caller,ok:=ctx.Deadline();ok&&caller.Before(deadline){deadline=caller};if err:=pipe.SetWriteDeadline(deadline);err!=nil{return errors.New("MCP_STDIO_WRITE_FAILED")}}
  _,err:=stdin.Write(raw);return err
 })
 return nil
}

func validEndpoint(u *url.URL)bool{return u!=nil&&(u.Scheme=="http"||u.Scheme=="https")&&u.Host!=""&&u.User==nil&&u.Fragment==""}
// startSSE implements the legacy 2024-11-05 SSE+POST transport, not the newer
// streamable-HTTP protocol. The desktop original explicitly rejects SSE; this
// native implementation is an extension, not a claimed original parity result.
func(c *connection)startSSE(caller context.Context)error{
 base,err:=url.Parse(c.config.URL);if err!=nil||!validEndpoint(base){return errors.New("MCP_SSE_ENDPOINT_INVALID")}
 // The administrator-configured URL grants precisely its origin, not the
 // process environment, arbitrary caller URLs, redirects or cross-origin POST.
 client,err:=egress.New([]string{base.Scheme+"://"+base.Host});if err!=nil{return errors.New("MCP_SSE_ENDPOINT_INVALID")}
 ready:=make(chan *url.URL,1);failed:=make(chan error,1)
 go func(){
  defer client.Close()
  req,err:=http.NewRequestWithContext(c.ctx,http.MethodGet,base.String(),nil)
  if err!=nil{failed<-errors.New("MCP_SSE_INVALID");return}
  req.Header.Set("Accept","text/event-stream")
  resp,err:=client.Do(req)
  if err!=nil{failed<-errors.New("MCP_CONNECT_FAILED");return}
  defer resp.Body.Close()
  media,_,_:=mime.ParseMediaType(resp.Header.Get("Content-Type"))
  if resp.StatusCode!=http.StatusOK||media!="text/event-stream"{failed<-errors.New("MCP_SSE_INVALID");return}
  c.mu.Lock();if !c.alive(){c.mu.Unlock();return};c.closeTransport=func(){_ = resp.Body.Close()};c.mu.Unlock()
  err=c.readSSE(resp.Body,base,ready)
  if err==nil{err=errors.New("MCP_SERVER_NOT_CONNECTED")}
  select{case failed<-err:default:}
  c.stop("error",errorCode(err,"MCP_SSE_INVALID"))
 }()
 timer:=time.NewTimer(c.options.RequestTimeout);defer timer.Stop()
 var endpoint *url.URL
 select{
 case endpoint=<-ready:
 case err=<-failed:return err
 case <-caller.Done():return errors.New("MCP_REQUEST_CANCELED")
 case <-timer.C:return errors.New("MCP_REQUEST_TIMEOUT")
 case <-c.ctx.Done():return errors.New("MCP_SERVER_NOT_CONNECTED")
 }
 go c.writer(func(caller context.Context,raw []byte)error{
  ctx,cancel:=context.WithTimeout(caller,c.options.RequestTimeout);defer cancel()
  stop:=context.AfterFunc(c.ctx,cancel);defer stop()
  if ctx.Err()!=nil||c.ctx.Err()!=nil{return errors.New("MCP_REQUEST_CANCELED")}
  req,err:=http.NewRequestWithContext(ctx,http.MethodPost,endpoint.String(),bytes.NewReader(raw));if err!=nil{return errors.New("MCP_SSE_POST_FAILED")}
  req.Header.Set("Content-Type","application/json")
  resp,err:=client.Do(req);if err!=nil{return errors.New("MCP_SSE_POST_FAILED")};defer resp.Body.Close()
  if resp.StatusCode<200||resp.StatusCode>=300{return errors.New("MCP_SSE_POST_FAILED")}
  // Legacy responses arrive only on SSE. Bound and drain an optional POST body.
  count,err:=io.Copy(io.Discard,io.LimitReader(resp.Body,MaxFrameBytes+1));if err!=nil||count>MaxFrameBytes{return errors.New("MCP_SSE_POST_FAILED")};return nil
 })
 return nil
}
func(c *connection)readSSE(reader io.Reader,base *url.URL,ready chan<- *url.URL)error{
 scanner:=bufio.NewScanner(reader);scanner.Buffer(make([]byte,4096),MaxFrameBytes+1)
 event:="";data:=make([]byte,0);size:=0;announced:=false
 dispatch:=func()error{
  if len(data)==0{return nil}
  body:=data[:len(data)-1]
  switch event{
  case "endpoint":
   if announced{return errors.New("MCP_SSE_ENDPOINT_INVALID")}
   endpoint,err:=base.Parse(string(body));if err!=nil||!validEndpoint(endpoint)||!strings.EqualFold(endpoint.Scheme,base.Scheme)||!strings.EqualFold(endpoint.Host,base.Host){return errors.New("MCP_SSE_ENDPOINT_INVALID")}
   announced=true;ready<-endpoint
  case "message","":if !announced{return errors.New("MCP_SSE_INVALID")};return c.receive(body)
  }
  return nil
 }
 for scanner.Scan(){
  line:=scanner.Bytes();size+=len(line)+1
  if size>MaxFrameBytes{return errors.New("MCP_STDOUT_LIMIT_EXCEEDED")}
  if !utf8.Valid(line){return errors.New("MCP_STDOUT_INVALID")}
  if len(line)==0{if err:=dispatch();err!=nil{return err};event="";data=data[:0];size=0;continue}
  if line[0]==':'{continue}
  key,value,found:=bytes.Cut(line,[]byte(":"));if !found{value=nil};value=bytes.TrimPrefix(value,[]byte(" "))
  switch string(key){case "event":event=string(value);case "data":data=append(data,value...);data=append(data,'\n')}
 }
 if scanner.Err()!=nil{return errors.New("MCP_STDOUT_LIMIT_EXCEEDED")}
 if size>0{return errors.New("MCP_SSE_INVALID")}
 return nil
}
