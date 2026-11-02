package generationowner

import (
 "encoding/json"
 "errors"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)

// This test consumes ONLY the disposable GitHub oracle artifact. Neither native
// test execution nor production invokes Node, reads author settings or .env.
func TestRealTypeScriptLeasePlanContract(t *testing.T){
 path:=os.Getenv("GO_GENERATION_OWNER_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("GO_GENERATION_OWNER_CONTRACT required in GitHub")};t.Skip("GitHub-only real TS oracle artifact")}
 if os.Getenv("GITHUB_ACTIONS")!="true"{t.Fatal("contract execution is GitHub-only")};temp:=os.Getenv("RUNNER_TEMP");if temp==""||path!=filepath.Join(temp,"novel-go-generation-owner","contract.json"){t.Fatal("unexpected contract path")}
 data,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
 var fixture struct{Version string `json:"version"`;Sources []struct{Path string `json:"path"`;Hash string `json:"sha256"`} `json:"sourceEvidence"`;Cases []struct{Name string `json:"name"`;Model Profile `json:"model"`;Receipt Receipt `json:"receipt"`;Task json.RawMessage `json:"task"`;Root int64 `json:"rootOutput"`;Plan json.RawMessage `json:"plan"`;Error string `json:"error"`;Decision json.RawMessage `json:"decision"`} `json:"cases"`;Invalid []json.RawMessage `json:"invalidTasks"`}
 if json.Unmarshal(data,&fixture)!=nil||fixture.Version!="native-generation-owner-foundation-v1"||len(fixture.Cases)<24||len(fixture.Sources)!=9{t.Fatal("incomplete contract")}
 for _,source:=range fixture.Sources{original,err:=os.ReadFile(filepath.Join("..","..","..",filepath.FromSlash(source.Path)));if err!=nil{t.Fatal(err)};if hashBytes(original)!=source.Hash{t.Fatalf("oracle source changed: %s",source.Path)}}
 for _,c:=range fixture.Cases{t.Run(c.Name,func(t *testing.T){
  receipt,err:=CreateReceipt(c.Model,c.Receipt.LeaseID,c.Receipt.CreatedAt,c.Receipt.ExpiresAt);if err!=nil{t.Fatal(err)};assertJSON(t,receipt,c.Receipt)
  task,err:=ParseTask(c.Task);if err!=nil{t.Fatal(err)};plan,err:=BuildPlan(c.Model,receipt,task,c.Root)
  if c.Error!=""{var split *PreflightError;if !errors.As(err,&split)||err.Error()!=c.Error{t.Fatalf("expected %s got %v",c.Error,err)};assertJSON(t,split.Decision,c.Decision);return};if err!=nil{t.Fatal(err)};assertJSON(t,plan,c.Plan)
 })}
 for _,raw:=range fixture.Invalid{if _,err:=ParseTask(raw);err==nil{t.Fatalf("invalid task accepted: %s",raw)}}
}
func assertJSON(t *testing.T,a,b any){t.Helper();decode:=func(v any)any{var out any;var raw []byte;if r,ok:=v.(json.RawMessage);ok{raw=r}else{var err error;raw,err=json.Marshal(v);if err!=nil{t.Fatal(err)}};if json.Unmarshal(raw,&out)!=nil{t.Fatal("bad JSON")};return out};left,right:=decode(a),decode(b);if !reflect.DeepEqual(left,right){x,_:=json.Marshal(left);y,_:=json.Marshal(right);t.Fatalf("native %s\nTS %s",x,y)}}
