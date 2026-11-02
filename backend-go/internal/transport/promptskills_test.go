package transport

import (
 "os"
 "path/filepath"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
)

func TestPromptSkillsHTTPCRUDAndNoHostPathsOrRawLocalInstall(t *testing.T){
 server:=fixture(t);root:=t.TempDir();if err:=os.Chmod(root,0700);err!=nil{t.Fatal(err)}
 service,err:=promptskills.New(root);if err!=nil{t.Fatal(err)};defer service.Close()
 directory:=filepath.Join(root,"skills","public-skill");if err:=os.MkdirAll(directory,0700);err!=nil{t.Fatal(err)}
 if err:=os.WriteFile(filepath.Join(directory,"SKILL.md"),[]byte("---\nname: public-skill\n---\nPublic skill content."),0600);err!=nil{t.Fatal(err)}
 if err:=server.BindPromptSkills(service,globalconfig.NewStartup(globalconfig.StartupCapture{Generation:"committed-generation"}));err!=nil{t.Fatal(err)}
 cookie,csrf:=bootstrap(t,server)
 save:=rpc(t,server,cookie,csrf,"prompt:save-global",`{"args":[{"key":"public","content":"Test prompt 中文"}]}`,nil)
 if save.Code!=200||!strings.Contains(save.Body.String(),`"success":true`){t.Fatal(save.Code,save.Body.String())}
 load:=rpc(t,server,cookie,csrf,"prompt:load-global",`{"args":[]}`,nil)
 if load.Code!=200||!strings.Contains(load.Body.String(),"Test prompt 中文"){t.Fatal(load.Code,load.Body.String())}
 skills:=rpc(t,server,cookie,csrf,"skills:list-user",`{"args":[]}`,nil)
 if skills.Code!=200||strings.Contains(skills.Body.String(),root)||!strings.Contains(skills.Body.String(),"managed://skills/public-skill"){t.Fatal(skills.Code,skills.Body.String())}
 invalid:=rpc(t,server,cookie,csrf,"prompt:save-global",`{"args":[{"key":"../outside","content":"no"}]}`,nil)
 if invalid.Code!=200||!strings.Contains(invalid.Body.String(),`"success":false`)||strings.Contains(invalid.Body.String(),root){t.Fatal(invalid.Code,invalid.Body.String())}
 local:=rpc(t,server,cookie,csrf,"skills:install-local",`{"args":["/private/host/path"]}`,nil)
 if local.Code!=501{t.Fatal("unqualified install was exposed",local.Code,local.Body.String())}
 deleted:=rpc(t,server,cookie,csrf,"prompt:delete-global",`{"args":["public","zh-CN"]}`,nil)
 if deleted.Code!=200||!strings.Contains(deleted.Body.String(),`"success":true`){t.Fatal(deleted.Body.String())}
 uninstall:=rpc(t,server,cookie,csrf,"skills:uninstall-user",`{"args":["public-skill"]}`,nil)
 if uninstall.Code!=200||!strings.Contains(uninstall.Body.String(),`"success":true`){t.Fatal(uninstall.Body.String())}
 if _,err:=os.Stat(directory);!os.IsNotExist(err){t.Fatal("skill not removed",err)}
}
