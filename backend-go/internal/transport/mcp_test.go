package transport

import (
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/mcp"
)

func TestMCPHTTPAdminGateBeforeConfigProcessAndNetwork(t *testing.T){
 server:=fixture(t);manager:=mcp.New(mcp.Options{ConfigPath:"/never-opened-config",RuntimeDir:"/never-created-runtime"});defer manager.Close()
 startup:=globalconfig.NewStartup(globalconfig.StartupCapture{Generation:"committed-generation"})
 if err:=server.BindMCP(manager,startup,func(Caller)bool{return false});err!=nil{t.Fatal(err)}
 if !manager.HasSecretRegistrar(){t.Fatal("MCP secrets not protected before admission")}
 cookie,csrf:=bootstrap(t,server)
 for operation,body:=range map[string]string{
  "mcp:load-config":`{"args":[]}`,"mcp:connect":`{"args":["server"]}`,"mcp:disconnect":`{"args":["server"]}`,"mcp:disconnect-all":`{"args":[]}`,"mcp:list-resources":`{"args":[]}`,"mcp:call-tool":`{"args":["server","tool",{}]}`,
 }{
  response:=rpc(t,server,cookie,csrf,operation,body,nil)
  if response.Code!=403||!strings.Contains(response.Body.String(),"MCP_SERVER_ADMIN_ONLY"){t.Fatal(operation,response.Code,response.Body.String())}
 }
 for _,operation:=range []string{"mcp:list-tools","mcp:get-servers-status"}{response:=rpc(t,server,cookie,csrf,operation,`{"args":[]}`,nil);if response.Code!=200||!strings.Contains(response.Body.String(),`"result":[]`){t.Fatal(operation,response.Code,response.Body.String())}}
 if manager.Enabled(){t.Fatal("browser operation enabled privileged tool execution")}
 internal:=rpc(t,server,cookie,csrf,"mcp:get-config-path",`{"args":[]}`,nil);if internal.Code!=404{t.Fatal("host config path exposed",internal.Code,internal.Body.String())}
}
func TestMCPHTTPBindingRefusesAlreadyEnabledManager(t *testing.T){
 server:=fixture(t);manager:=mcp.New(mcp.Options{TrustedAdminGrant:true});defer manager.Close();if err:=manager.SetEnabled(true);err!=nil{t.Fatal(err)}
 if err:=server.BindMCP(manager,globalconfig.NewStartup(globalconfig.StartupCapture{Generation:"admitted"}),func(Caller)bool{return true});err==nil{t.Fatal("enabled manager admitted after secret protection boundary")}
}
