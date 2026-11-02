package generation

import (
 "context"
 "database/sql"
 "encoding/json"
 "io"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "testing"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func providerLedger(t *testing.T)*store.GenerationLedger {
 t.Helper();oracle:=os.Getenv("GO_SQLITE_CONTRACT");if oracle=="" {if os.Getenv("GITHUB_ACTIONS")=="true" {t.Fatal("actual baseline required")};t.Skip("GitHub original schema fixture")}
 manifestBytes,err:=os.ReadFile(oracle);if err!=nil {t.Fatal(err)}
 var manifest struct{Fixtures []struct{Name string `json:"name"`;File string `json:"file"`} `json:"fixtures"`};if err:=json.Unmarshal(manifestBytes,&manifest);err!=nil {t.Fatal(err)}
 source:="";for _,fixture:=range manifest.Fixtures {if fixture.Name=="baseline-v7" {source=filepath.Join(filepath.Dir(oracle),fixture.File);break}};if source=="" {t.Fatal("baseline-v7 missing")}
 input,err:=os.ReadFile(source);if err!=nil {t.Fatal(err)}
 file:=filepath.Join(t.TempDir(),"provider-ledger.db");if err:=os.WriteFile(file,input,0600);err!=nil {t.Fatal(err)}
 dsn,err:=store.ExistingDSN(file);if err!=nil {t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil {t.Fatal(err)};t.Cleanup(func(){db.Close()})
 if _,err:=db.Exec(`DELETE FROM generation_artifacts; DELETE FROM generation_attempts; DELETE FROM generation_runs; DELETE FROM generation_roots`);err!=nil {t.Fatal(err)}
 return store.NewGenerationLedger(db,func()int64{return 1000},nil)
}

func TestNativeProviderStreamsIntoActualLedgerWithoutReasoningOrDoubleCount(t *testing.T) {
 for _,adapter:=range []string{"openai","gemini"} {t.Run(adapter,func(t *testing.T){
  ledger:=providerLedger(t)
  h:=strings.Repeat("a",64)
  // Ordered JSON remains intact from binding through reservation and artifacts.
  raw:=`[{"projectId":"provider-project","epoch":"provider-epoch","operation":"chapter-draft","uiActionNonce":"provider-action","frozenInputHash":"`+h+`","fingerprint":{"chapterBriefHash":"`+h+`","authorGuidanceHash":"`+h+`","dependencyHash":"`+h+`","contextSnapshotHash":"`+h+`","templateHash":"`+h+`","skillSnapshotHash":"`+h+`","modelLeaseRevision":"`+h+`","policyHash":"`+h+`","outputContractHash":"`+h+`"},"contextSnapshotId":"provider-context","sourceManifest":{"purpose":"synthetic-provider"},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":1,"maxOutputPerRequest":100,"maxActiveElapsedMs":60000}}]`
  opened,err:=ledger.CallJSON(context.Background(),"open",json.RawMessage(raw));if err!=nil {t.Fatal(err)}
  run:=opened.(map[string]any)["runId"].(string)
  wire:="data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"PRIVATE-REASONING\",\"content\":\"正文🙂\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"完\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\ndata: [DONE]\n\n"
  if adapter=="gemini" {wire="data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"PRIVATE-REASONING\",\"thought\":true},{\"text\":\"正文🙂完\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":3,\"thoughtsTokenCount\":2,\"totalTokenCount\":15}}\n\n"}
  requests:=0
  server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
   requests++;if r.Method!="POST" {t.Error(r.Method)};body,err:=io.ReadAll(r.Body);if err!=nil||!strings.Contains(string(body),"synthetic prompt") {t.Error("missing native request",err)}
   w.Header().Set("Content-Type","text/event-stream");_,_=io.WriteString(w,wire)
  }));defer server.Close()
  reasoning:="included-in-completion";if adapter=="gemini" {reasoning="separately-billed"}
  dispatch:=NativeProvider(server.Client().Do)
  service:=New(Dependencies{Ledger:ledger,Dispatch:dispatch})
  receipt,err:=service.Execute(context.Background(),Request{RunID:run,InvocationNonce:"provider-invocation",RequestHash:h,ReservedTokens:401,RequestedOutputTokens:100,InputUpperBoundTokens:100,ReasoningUpperBoundTokens:100,UsagePolicy:map[string]any{"estimatorVersion":"synthetic","safetyMarginTokens":1,"reasoning":reasoning,"canBoundTotalLiability":true},ProviderRequest:ProviderRequest{Adapter:adapter,Model:llm.Model{Provider:adapter,BaseURL:server.URL,ModelName:"synthetic-model",APIKey:"synthetic-provider-secret",MaxTokens:100},Messages:[]llm.Message{{Role:"user",Content:"synthetic prompt"}}}})
  if err!=nil {t.Fatal(err)}
  artifact:=receipt["artifact"].(map[string]any);if artifact["text"]!="正文🙂完" {t.Fatal("provider text mismatch",artifact)}
  result:=receipt["result"].(map[string]any);usage:=result["usage"].(map[string]any)
  if usage["trusted"]!=true||usage["actualTokens"]!=float64(15) {t.Fatal("usage double counted or not trusted",usage)}
  encoded,err:=json.Marshal(receipt);if err!=nil||strings.Contains(string(encoded),"PRIVATE-REASONING")||strings.Contains(string(encoded),"synthetic-provider-secret")||strings.Contains(string(encoded),"synthetic prompt") {t.Fatal("sensitive provider input persisted",err)}
  replay,err:=service.Execute(context.Background(),Request{RunID:run,InvocationNonce:"provider-invocation",RequestHash:h})
  if err!=nil||replay["artifact"].(map[string]any)["text"]!="正文🙂完"||requests!=1 {t.Fatal("durable replay redispatched",requests,err)}
 })}
}

func TestNativeProviderRejectsUnknownRequestsAndSanitizesFailure(t *testing.T) {
 dispatch:=NativeProvider(func(*http.Request)(*http.Response,error){t.Fatal("unsupported input dispatched");return nil,nil})
 if _,err:=dispatch(context.Background(),map[string]any{},Callbacks{});err==nil||err.Error()!="GENERATION_PROVIDER_REQUEST_INVALID" {t.Fatal(err)}
 for _,test:=range []struct{status int;wire,want string}{{400,"maximum context length is 8192 tokens PRIVATE-SECRET","GENERATION_CONTEXT_LENGTH_EXCEEDED"},{200,"data: {\"choices\":[]}\n\n","NETWORK_ERROR"},{500,"PRIVATE-SECRET","GENERATION_PROVIDER_FAILED"}} {
  native:=NativeProvider(func(*http.Request)(*http.Response,error){return &http.Response{StatusCode:test.status,Body:io.NopCloser(strings.NewReader(test.wire))},nil})
  _,err:=native(context.Background(),ProviderRequest{Adapter:"openai",Model:llm.Model{Provider:"openai",BaseURL:"https://synthetic.invalid",ModelName:"synthetic",MaxTokens:10}},Callbacks{})
  if err==nil||err.Error()!=test.want {t.Fatalf("code=%v want=%s",err,test.want)}
 }
}
