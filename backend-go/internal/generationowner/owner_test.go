package generationowner

import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/secrets"
)

// publicScope carries BOTH identities a real admission captures: the per-tab
// receipt identity (Caller/Session/Tab) that a lease is scoped to, and the LEASE
// OWNER (LeaseSession/LeaseClient) Current re-proves it against.
var publicScope=Scope{Caller:"public-caller",Session:"public-session",Tab:"public-tab",LeaseSession:"public-lease-session",LeaseClient:"public-lease-client",Project:"public-project",Epoch:"public-epoch"}
func publicProfile()Profile{return Profile{ID:"public-model",Provider:"custom",Protocol:"openai",BaseURL:"https://example.invalid/v1",ModelName:"public-synthetic",Temperature:0.8,MaxTokens:8192,Purposes:[]string{"generation"}}}
func publicTask()Task{return Task{Purpose:"public-test",Output:"visible-text",Messages:[]llm.Message{{Role:"user",Content:"PUBLIC synthetic 合成 😀"}}}}
type testSettings struct { mu sync.RWMutex; gate sync.Mutex; model Profile; epoch Scope; now atomic.Int64 }
func newSettings(t *testing.T)(*testSettings,*Registry){t.Helper();s:=&testSettings{model:publicProfile(),epoch:publicScope};s.now.Store(1000);r,err:=NewRegistry(RegistryOptions{
 LoadModel:func(_ context.Context,id string)(Profile,bool,error){s.mu.RLock();defer s.mu.RUnlock();return cloneProfile(s.model),id==s.model.ID,nil},
 Current:func(_ context.Context,scope Scope)error{s.mu.RLock();defer s.mu.RUnlock();if scope!=s.epoch{return failure("stale")};return nil},
 AuthorityLock:func(ctx context.Context,_ Scope)(func(),error){if ctx.Err()!=nil{return nil,ctx.Err()};s.gate.Lock();return s.gate.Unlock,nil},
 Now:func()time.Time{return time.UnixMilli(s.now.Load())},TTL:time.Second,
 });if err!=nil{t.Fatal(err)};return s,r}
func begin(t *testing.T,r *Registry)Receipt{t.Helper();receipt,err:=r.Begin(context.Background(),publicScope,"public-model","generation");if err!=nil{t.Fatal(err)};return receipt}
func TestLeaseIdentityExpiryAndDetachedReceipt(t *testing.T){
 s,r:=newSettings(t);receipt:=begin(t,r);receipt.CapabilityEvidence.Output=1
 resolved,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");if err!=nil||resolved.CapabilityEvidence.Output!=8192{t.Fatalf("alias %v %v",resolved,err)}
 foreign:=[]Scope{publicScope,publicScope,publicScope,publicScope,publicScope,publicScope,publicScope};foreign[0].Caller="other";foreign[1].Session="other";foreign[2].Tab="other";foreign[3].Project="other";foreign[4].Epoch="browser-lease-is-not-epoch";foreign[5].LeaseSession="other";foreign[6].LeaseClient="other"
 for _,scope:=range foreign{if _,err:=r.Resolve(context.Background(),receipt.LeaseID,scope,"generation");err==nil{t.Fatalf("foreign scope accepted %+v",scope)}}
 if _,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"embedding");err==nil{t.Fatal("foreign purpose accepted")};s.now.Store(2000);if _,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");err==nil||err.Error()!="MODEL_EXECUTION_LEASE_EXPIRED"{t.Fatalf("expiry %v",err)}
}
func TestRevisionCredentialAndEpochInvalidate(t *testing.T){
 changes:=[]func(*testSettings){func(s *testSettings){s.model.MaxTokens++},func(s *testSettings){s.model.Temperature=0.4},func(s *testSettings){s.model.ModelName="new"},func(s *testSettings){s.model.BaseURL="https://other.invalid/v1"},func(s *testSettings){s.model.EncryptedCredential=[]byte("PUBLIC different envelope")},func(s *testSettings){s.epoch.Epoch="new-epoch"}}
 for i,change:=range changes{t.Run(string(rune('a'+i)),func(t *testing.T){s,r:=newSettings(t);receipt:=begin(t,r);s.gate.Lock();s.mu.Lock();change(s);s.mu.Unlock();s.gate.Unlock();if _,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");err==nil{t.Fatal("stale model/scope accepted")}})}
}
// TestReceiptStaysScopedToTheTabThatTookIt is the multi-tab half of the receipt
// identity, and the property the production lane and the import-run receipt scope
// both rely on: two browser tabs of ONE account share the LEASE OWNER
// (LeaseSession/LeaseClient), and a model lease one of them took must still not be
// resolvable, closeable or reproducible by its sibling. Only the per-tab half of
// the Scope (Caller/Session/Tab) differs here.
func TestReceiptStaysScopedToTheTabThatTookIt(t *testing.T){
 _,r:=newSettings(t);receipt:=begin(t,r)
 sibling:=publicScope;sibling.Caller="public-sibling-session";sibling.Session="public-sibling-session";sibling.Tab="public-sibling-tab"
 if _,err:=r.Resolve(context.Background(),receipt.LeaseID,sibling,"generation");err==nil{t.Fatal("a sibling tab of the same account resolved this tab's receipt")}
 if r.Close(context.Background(),receipt.LeaseID,sibling,"generation"){t.Fatal("a sibling tab of the same account closed this tab's receipt")}
 if _,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");err!=nil{t.Fatalf("the tab that took the receipt lost it: %v",err)}
}
func TestConcurrentRegistryAndRevocation(t *testing.T){
 _,r:=newSettings(t);var wg sync.WaitGroup;for i:=0;i<32;i++{wg.Add(1);go func(){defer wg.Done();receipt,err:=r.Begin(context.Background(),publicScope,"public-model","generation");if err!=nil{t.Error(err);return};_,_ = r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");r.Close(context.Background(),receipt.LeaseID,publicScope,"generation")}()};wg.Wait();receipt:=begin(t,r);r.RevokeScope(publicScope);if _,err:=r.Resolve(context.Background(),receipt.LeaseID,publicScope,"generation");err==nil{t.Fatal("revocation failed")}
}
func TestUnsupportedEndpointsAndModelPolicies(t *testing.T){
 for _,endpoint:=range []string{"https://example.invalid/a%2Fb","https://example.invalid/a/../b","https://example.invalid/a?credential=x","https://example.invalid/#frag","https://user:pass@example.invalid","file:///tmp/not-allowed"}{p:=publicProfile();p.BaseURL=endpoint;if _,err:=CreateReceipt(p,"public",1,2);err==nil{t.Fatalf("unsupported URL accepted %s",endpoint)}}
 for _,name:=range []string{"o3","gpt-5","gemini-2.5-pro"}{p:=publicProfile();p.BaseURL="https://api.openai.com/v1";p.ModelName=name;if _,err:=CreateReceipt(p,"public",1,2);err==nil{t.Fatal("unported catalog accepted")}}
 p:=publicProfile();p.ReasoningMapping=json.RawMessage(`{"adapter":"openai-thinking-budget"}`);if _,err:=CreateReceipt(p,"public",1,2);err==nil{t.Fatal("manual reasoning mapping accepted")}
}
func TestFrozenDispatchNativeProviderAndEgress(t *testing.T){
 var hits atomic.Int64;server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,req *http.Request){hits.Add(1);if req.Header.Get("Authorization")!="Bearer PUBLIC synthetic credential"{t.Error("wrong credential")};if req.URL.Path!="/v1/chat/completions"{t.Error("wrong provider path")};w.Header().Set("Content-Type","text/event-stream");_,_ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"PUBLIC result\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}));defer server.Close()
 s,r:=newSettings(t);storage,err:=secrets.New(make([]byte,32));if err!=nil{t.Fatal(err)};cipher,err:=storage.EncryptString("PUBLIC synthetic credential");if err!=nil{t.Fatal(err)};s.model.BaseURL=server.URL+"/v1";s.model.EncryptedCredential=cipher
 client,err:=egress.New([]string{server.URL});if err!=nil{t.Fatal(err)};defer client.Close();executor:=&Executor{Registry:r,SecretStorage:storage,Egress:client};receipt:=begin(t,r);task:=publicTask();frozen,err:=executor.Prepare(context.Background(),publicScope,receipt.LeaseID,task,1024,nil);if err!=nil{t.Fatal(err)}
 task.Messages[0].Content="mutated";projection:=frozen.Plan();*projection.Options.MaxTokens=1;if frozen.Plan().Output!=1024||*frozen.Plan().Options.MaxTokens!=1024{t.Fatal("plan alias")}
 var visible strings.Builder;result,err:=executor.Dispatch(context.Background(),frozen,generation.Callbacks{OnVisible:func(e generation.VisibleEvent){visible.WriteString(e.Text)}});if err!=nil||result.FinishReason!="stop"||visible.String()!="PUBLIC result"||hits.Load()!=1{t.Fatalf("dispatch %v %+v %q hits=%d",err,result,visible.String(),hits.Load())}
 b,_:=json.Marshal(frozen);if string(b)!="{}"{t.Fatal("private frozen state leaked")};b,_=json.Marshal(receipt);if strings.Contains(string(b),"credential")||strings.Contains(string(b),server.URL){t.Fatal("receipt leaked secret or URL")}
 if _,err:=executor.Dispatch(context.Background(),map[string]any{"apiKey":"raw","baseUrl":server.URL},generation.Callbacks{});err==nil{t.Fatal("raw RPC accepted")}
 s.mu.Lock();s.model.MaxTokens++;s.mu.Unlock();if _,err:=executor.Dispatch(context.Background(),frozen,generation.Callbacks{});err==nil||hits.Load()!=1{t.Fatal("stale dispatch reached server")}
}
func TestActualDispatchRepeatValidationAndMandatoryGate(t *testing.T){
 s,r:=newSettings(t);storage,_:=secrets.New(make([]byte,32));cipher,_:=storage.EncryptString("PUBLIC");s.model.EncryptedCredential=cipher;client,_:=egress.New([]string{"https://example.invalid"});defer client.Close();executor:=&Executor{Registry:r,SecretStorage:storage,Egress:client};receipt:=begin(t,r);frozen,err:=executor.Prepare(context.Background(),publicScope,receipt.LeaseID,publicTask(),1024,nil);if err!=nil{t.Fatal(err)}
 r.options.AuthorityLock=nil;if _,err:=executor.Dispatch(context.Background(),frozen,generation.Callbacks{});err==nil||err.Error()!="MODEL_EXECUTION_CONFIG_REQUIRED"{t.Fatal("missing dispatch gate accepted")}
 r.options.AuthorityLock=func(context.Context,Scope)(func(),error){s.mu.Lock();s.model.MaxTokens++;s.mu.Unlock();return func(){},nil}
 if _,err:=executor.Dispatch(context.Background(),frozen,generation.Callbacks{});err==nil{t.Fatal("model changed between prepare and HTTP not revalidated")}
}
func TestSourceCapabilityBindingAndChange(t *testing.T){
 _,r:=newSettings(t);receipt:=begin(t,r);value:=SourceValue{Revision:2,Text:"PUBLIC source bytes 合成"};reads:=0;read:=func(context.Context,Scope,SourceKey)(SourceValue,error){reads++;return value,nil};sources:=Sources{ProjectCore:read,Blueprint:read,Draft:read,Prompt:read}
 binding,err:=r.BindSources(context.Background(),publicScope,receipt.LeaseID,sources,[]SourceKey{{"project-core","main"},{"blueprint","1"},{"draft","2"},{"prompt","chapter_draft:zh-CN"}});if err!=nil||reads!=8{t.Fatalf("bind %v reads=%d",err,reads)};refs:=binding.Refs();refs[0].ContentHash="mutated";if binding.Refs()[0].ContentHash=="mutated"{t.Fatal("ref alias")};value.Text+=" changed";if binding.validate(context.Background())==nil{t.Fatal("source change ignored")}
 for _,key:=range []SourceKey{{"kb","1"},{"finalized","1"},{"portable","1"},{"skill","project:x"},{"draft","../escape"},{"prompt","https://bad"}}{if _,err:=r.BindSources(context.Background(),publicScope,receipt.LeaseID,sources,[]SourceKey{key});err==nil{t.Fatalf("unsupported source accepted %+v",key)}}
 if _,err:=r.BindSources(context.Background(),publicScope,receipt.LeaseID,Sources{},[]SourceKey{{"project-core","main"}});err==nil{t.Fatal("missing capability accepted")}
}
func TestTaskRejectsProviderOverridesAndNullContent(t *testing.T){
 for _,raw:=range []string{`{"purpose":"ok","output":"visible-text","messages":[{"role":"user","content":null}]}`,`{"purpose":"ok","output":"visible-text","messages":[{"role":"user","content":"PUBLIC"}],"apiKey":"x"}`,`{"purpose":"ok","output":"visible-text","messages":[{"role":"user","content":"PUBLIC"}],"maxTokens":100000}`} {if _,err:=ParseTask([]byte(raw));err==nil{t.Fatal("invalid semantic task accepted")}}
}
