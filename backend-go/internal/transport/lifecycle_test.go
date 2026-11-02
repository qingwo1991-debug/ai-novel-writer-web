package transport

import (
 "io"
 "net/http/httptest"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"
)

type blockedBody struct{ started chan struct{};release chan struct{};once sync.Once;reader io.Reader }
func(b *blockedBody)Read(bytes []byte)(int,error){b.once.Do(func(){close(b.started);<-b.release});return b.reader.Read(bytes)}
func(b *blockedBody)Close()error{return nil}

func TestSlowRequestPinsTabBeforeBodyRead(t *testing.T){
 server:=fixture(t);var nanos atomic.Int64;nanos.Store(time.Now().UnixNano());server.now=func()time.Time{return time.Unix(0,nanos.Load())}
 cookie,csrf:=bootstrap(t,server)
 body:=&blockedBody{started:make(chan struct{}),release:make(chan struct{}),reader:strings.NewReader(`{"args":[]}`)}
 request:=httptest.NewRequest("POST","/api/operations/config:get",nil);request.Body=body;request.Header.Set("X-Novel-User","synthetic-author");request.Header.Set("X-Client-Id","synthetic-tab-0001");request.Header.Set("Origin","http://synthetic.test");request.Header.Set("X-CSRF-Token",csrf);request.Header.Set("Content-Type","application/json");request.AddCookie(cookie)
 done:=make(chan *httptest.ResponseRecorder,1);go func(){response:=httptest.NewRecorder();server.ServeHTTP(response,request);done<-response}()
 <-body.started
 server.mu.Lock();var original *client;for _,entry:=range server.clients{original=entry};if original==nil||original.inflight!=1||original.pinned!=1{server.mu.Unlock();t.Fatal("request not fenced before reading")};server.mu.Unlock()
 nanos.Add(int64(3*time.Minute))
 response:=rpc(t,server,cookie,csrf,"config:get",`{"args":[]}`,nil);if response.Code!=501{t.Fatal(response.Code)}
 server.mu.Lock();if len(server.clients)!=1{server.mu.Unlock();t.Fatal("tab replaced")};for _,entry:=range server.clients{if entry!=original{server.mu.Unlock();t.Fatal("detached slow request")}};server.mu.Unlock()
 close(body.release);response=<-done;if response.Code!=501{t.Fatal(response.Code)}
 server.mu.Lock();defer server.mu.Unlock();if original.inflight!=0||original.pinned!=0{t.Fatal("fences leaked",original.inflight,original.pinned)}
}

func TestRequestLimitIncludesBodiesNotYetParsed(t *testing.T){
 server:=fixture(t);cookie,csrf:=bootstrap(t,server)
 // White-box reserve mimics 32 admitted slow bodies without creating timers or
 // sleeping. The 33rd request must fail before calling its request body reader.
 _ = rpc(t,server,cookie,csrf,"config:get",`{"args":[]}`,nil)
 server.mu.Lock();var current *client;for _,entry:=range server.clients{current=entry};current.inflight=32;server.mu.Unlock()
 response:=rpc(t,server,cookie,csrf,"config:get",`not-json`,nil)
 if response.Code!=429||!strings.Contains(response.Body.String(),"REQUEST_LIMIT"){t.Fatal(response.Code,response.Body.String())}
 server.mu.Lock();current.inflight=0;if current.pinned!=0{t.Fatal("request fence leaked")};server.mu.Unlock()
}
