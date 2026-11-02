package transport

import (
 "bufio"
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)
func TestFixedNavigationThroughAuthenticatedRPCAndPrivateSSE(t *testing.T){
 server:=fixture(t);if err:=server.BindNavigation();err!=nil{t.Fatal(err)}
 testServer:=httptest.NewServer(server);defer testServer.Close()
 cookie,csrf:=bootstrap(t,server)
 ctx,cancel:=context.WithCancel(context.Background());defer cancel()
 request,_:=http.NewRequestWithContext(ctx,"GET",testServer.URL+"/api/events?client=synthetic-tab-0001",nil);request.Header.Set("X-Novel-User","synthetic-author");request.AddCookie(cookie)
 response,err:=testServer.Client().Do(request);if err!=nil{t.Fatal(err)};defer response.Body.Close()
 reader:=bufio.NewReader(response.Body);if _,err:=reader.ReadString('\n');err!=nil{t.Fatal(err)};if _,err:=reader.ReadString('\n');err!=nil{t.Fatal(err)}
 for _,test:=range []struct{op,body,url string}{
  {"official-homepage:open",`{"args":[]}`,officialHomepage},
  {"model-provider-resource:open",`{"args":["siliconflow-console"]}`,providerResources["siliconflow-console"]},
  {"model-provider-resource:open",`{"args":["siliconflow-invite"]}`,providerResources["siliconflow-invite"]},
  {"model-provider-resource:open",`{"args":["siliconflow-docs"]}`,providerResources["siliconflow-docs"]},
 }{
  result:=rpc(t,server,cookie,csrf,test.op,test.body,nil);if result.Code!=200||!strings.Contains(result.Body.String(),`"success":true`){t.Fatal(result.Code,result.Body.String())}
  line,err:=reader.ReadString('\n');if err!=nil||!strings.HasPrefix(line,"id: "){t.Fatal(line,err)}
  line,err=reader.ReadString('\n');if err!=nil{t.Fatal(err)}
  var event struct{Channel string `json:"channel"`;Args []struct{URL string `json:"url"`} `json:"args"`}
  if err:=json.Unmarshal([]byte(strings.TrimPrefix(line,"data: ")),&event);err!=nil{t.Fatal(err)}
  if event.Channel!="browser:open-external"||len(event.Args)!=1||event.Args[0].URL!=test.url{t.Fatal(event)}
  if _,err:=reader.ReadString('\n');err!=nil{t.Fatal(err)}
 }
 cancel()
}
func TestNavigationRejectsURLsAndDoesNotLeakAcrossTabs(t *testing.T){
 server:=fixture(t);if err:=server.BindNavigation();err!=nil{t.Fatal(err)};cookie,csrf:=bootstrap(t,server)
 for _,body:=range []string{`{"args":[]}`,`{"args":[null]}`,`{"args":[{}]}`,`{"args":["https://evil.invalid"]}`,`{"args":["__proto__"]}`} {
  result:=rpc(t,server,cookie,csrf,"model-provider-resource:open",body,nil)
  if result.Code!=200||!strings.Contains(result.Body.String(),"Unsupported model provider resource."){t.Fatal(result.Code,result.Body.String())}
 }
 // Establish another tab's hub, then confirm the first tab's fixed intent was
 // never added there. History is inspected without timing-based negative waits.
 _=rpc(t,server,cookie,csrf,"config:get",`{"args":[]}`,map[string]string{"X-Client-Id":"synthetic-other-tab"})
 server.mu.Lock();var other *client;for key,current:=range server.clients{if strings.HasSuffix(key,":synthetic-other-tab"){other=current}};server.mu.Unlock()
 if other==nil{t.Fatal("missing tab")};if err:=other.hub.Publish("synthetic-marker",nil);err!=nil{t.Fatal(err)}
 result:=rpc(t,server,cookie,csrf,"official-homepage:open",`{"args":[]}`,nil);if result.Code!=200{t.Fatal(result.Body.String())}
 history,_,cancel,err:=other.hub.Subscribe(1);if err!=nil{t.Fatal(err)};defer cancel();if len(history)!=0{t.Fatal("cross-tab event leak")}
 bad:=rpc(t,server,cookie,csrf,"official-homepage:open",`{"args":[]}`,map[string]string{"X-CSRF-Token":"bad"});if bad.Code!=403{t.Fatal(bad.Code)}
}
