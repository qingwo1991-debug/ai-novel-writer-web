package projectaccess

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "testing"
)
func TestPreflightKnowledgeOriginalCopyCurrentOracle(t *testing.T){
 path:=os.Getenv("GO_KNOWLEDGECOPY_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original copy oracle mandatory")};t.Skip("GitHub synthetic original contract required")}
 raw,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
 var contract struct{Format int `json:"format"`;Cases []struct{Name string `json:"name"`;Raw string `json:"raw"`;Current bool `json:"current"`;Dirty bool `json:"dirty"`;Edited bool `json:"edited"`;HashMismatch bool `json:"hashMismatch"`;RawSHA256 string `json:"rawSHA256"`} `json:"cases"`}
 if err:=json.Unmarshal(raw,&contract);err!=nil||contract.Format!=1||len(contract.Cases)!=7{t.Fatal("invalid original oracle",err)}
 flag:=func(value bool)int{if value{return 1};return 0}
 for _,item:=range contract.Cases{t.Run(item.Name,func(t *testing.T){c:=knowledgeAccessFixture(t);file:=filepath.Join(c.ProjectPath,".ai-novel/knowledge-copies/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json");writeAccess(t,file,[]byte(item.Raw));report,err:=Preflight(context.Background(),c);if err!=nil{t.Fatal(err)};if report.Knowledge==nil{t.Fatal("missing knowledge evidence")};summary:=report.Knowledge.Metadata;if summary.Copies!=1||summary.Current!=flag(item.Current)||summary.Dirty!=flag(item.Dirty)||summary.Edited!=flag(item.Edited)||summary.HashMismatch!=flag(item.HashMismatch){t.Fatal("original copy predicate mismatch",summary)};accessClean(t,c)})}
}
