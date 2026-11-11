// Package generationowner is an internal model-execution/provenance foundation.
// It is NOT the durable main-generation owner, an RPC surface, a workflow
// implementation, or authority to write author content. All dependencies are
// explicitly injected server capabilities; browser leases are never epochs.
package generationowner

import (
 "bytes"
 "context"
 "crypto/rand"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "net/url"
 "strings"
 "sync"
 "time"
)

func failure(code string) error { return errors.New(code) }
const safeInteger int64 = 9007199254740991

// Scope is captured by authenticated server admission, NOT decoded from RPC.
// Current must validate the captured project writer/session/tab lifetime.
//
// It carries TWO identities on purpose, and they must never be aliased:
//
//   - Caller/Session/Tab are the TAB IDENTITY: the browser cookie session plus
//     the tab. Registry receipts are scoped to them, so a model lease one tab
//     took cannot be resolved, closed or stolen by another tab of the same
//     account.
//   - LeaseSession/LeaseClient are the LEASE OWNER: the identity the project's
//     single-writer lease is held under, spelled the way projectsession.Owner
//     spells it so a Current implementation can rebuild the exact owner the
//     project was opened under. The production lane derives it from the
//     authenticated account (projectsession.AccountOwner), so every tab of one
//     author names the SAME live lease here while their Caller/Session/Tab still
//     differ; the migration scaffold keeps its per-tab pair. They are advisory
//     identity, not a capability: ownership is still proved by the runtime's own
//     lease fence, never by these strings.
type Scope struct {
	Caller       string
	Session      string
	Tab          string
	LeaseSession string
	LeaseClient  string
	Project      string
	Epoch        string
}
func (s Scope) valid() bool { return s.Caller!="" && s.Session!="" && s.Tab!="" && s.LeaseSession!="" && s.LeaseClient!="" && s.Project!="" && s.Epoch!="" }

// Profile is loaded only from trusted server settings. Raw JSON retains the
// original nested field order required by the TS revision hash. Credential data
// never enters receipts, JSON serialization, errors, or the plan projection.
type Profile struct {
 ID string `json:"id"`
 Provider string `json:"provider"`
 Protocol string `json:"protocol"`
 BaseURL string `json:"baseUrl"`
 ModelName string `json:"modelName"`
 Temperature float64 `json:"temperature"`
 MaxTokens int64 `json:"maxTokens"`
 Capabilities json.RawMessage `json:"capabilities,omitempty"`
 ReasoningOverride string `json:"reasoningOverride,omitempty"`
 ReasoningMapping json.RawMessage `json:"reasoningMapping,omitempty"`
 Purposes []string `json:"purposes"`
 EmbeddingOptions json.RawMessage `json:"embeddingOptions,omitempty"`
 EncryptedCredential []byte `json:"-"`
}
type capabilityHints struct {
 Context *int64 `json:"contextWindowTokens"`
 Output int64 `json:"maxOutputTokens"`
}
type EvidenceSource struct {
 Context string `json:"contextWindowTokens"`
 Output string `json:"maxOutputTokens"`
 Features string `json:"featureFlags"`
}
type Evidence struct {
 Source EvidenceSource `json:"source"`
 SubjectFingerprint string `json:"subjectFingerprint"`
 Context *int64 `json:"contextWindowTokens"`
 Output int64 `json:"maxOutputTokens"`
 Reasoning *bool `json:"reasoning"`
 StructuredOutput *bool `json:"structuredOutput"`
 Usage *bool `json:"usage"`
}
type Receipt struct {
 LeaseID string `json:"leaseId"`
 ModelID string `json:"modelId"`
 Provider string `json:"provider"`
 Protocol string `json:"protocol"`
 ModelName string `json:"modelName"`
 ModelRevision string `json:"modelRevision"`
 EndpointFingerprint string `json:"endpointFingerprint"`
 CapabilityEvidence Evidence `json:"capabilityEvidence"`
 CreatedAt int64 `json:"createdAt"`
 ExpiresAt int64 `json:"expiresAt"`
}
func clone[T any](v T) T { b,_:=json.Marshal(v); var out T; _=json.Unmarshal(b,&out); return out }
func cloneProfile(p Profile) Profile { out:=clone(p); out.EncryptedCredential=append([]byte(nil),p.EncryptedCredential...); return out }
func hashBytes(b []byte) string { h:=sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func hashJSON(v any) string { var b bytes.Buffer; e:=json.NewEncoder(&b); e.SetEscapeHTML(false); if e.Encode(v)!=nil{return ""}; raw:=bytes.TrimSuffix(b.Bytes(),[]byte{'\n'}); raw=bytes.ReplaceAll(raw,[]byte(`\u2028`),[]byte("\u2028"));raw=bytes.ReplaceAll(raw,[]byte(`\u2029`),[]byte("\u2029")); return hashBytes(raw) }
func present(raw json.RawMessage) bool { return len(raw)>0 && string(raw)!="null" }
func positive(v int64) bool { return v>0 && v<=safeInteger }
func min(a,b int64) int64 { if a<b{return a};return b }

func normalizedEndpoint(value string) (string,error) {
 u,err:=url.Parse(strings.TrimSpace(value));if err!=nil||u==nil||u.Host==""||u.Opaque!=""||u.RawPath!=""||(u.Scheme!="https"&&u.Scheme!="http")||u.User!=nil{return "",failure("MODEL_ENDPOINT_INVALID")}
 // Do not silently drop query/fragment routing semantics at native execution.
 if u.RawQuery!=""||u.ForceQuery||u.Fragment!=""||strings.Contains(value,"#"){return "",failure("MODEL_ENDPOINT_INVALID")}
 u.Host=strings.ToLower(u.Host)
 if u.Scheme=="https"&&u.Port()=="443"||u.Scheme=="http"&&u.Port()=="80" {u.Host=u.Hostname();if strings.Contains(u.Host,":"){u.Host="["+u.Host+"]"}}
 for _,part:=range strings.Split(u.Path,"/"){if part=="."||part==".."{return "",failure("MODEL_ENDPOINT_INVALID")}}
 u.Path=strings.TrimRight(u.Path,"/");u.RawPath=""
 return strings.TrimSuffix(u.String(),"/"),nil
}
type endpointSubject struct { Provider string `json:"provider"`; Protocol string `json:"protocol"`; Endpoint string `json:"endpoint"` }
func subject(p Profile)(endpointSubject,error){endpoint,err:=normalizedEndpoint(p.BaseURL);return endpointSubject{p.Provider,p.Protocol,endpoint},err}
func modelRevision(p Profile)(string,error){
 endpoint,err:=normalizedEndpoint(p.BaseURL);if err!=nil{return "",err};override:=p.ReasoningOverride;if override==""{override="auto"}
 caps:=p.Capabilities;if !present(caps){caps=json.RawMessage("null")};embedding:=p.EmbeddingOptions;if !present(embedding){embedding=json.RawMessage("null")}
 // Field order is the original modelRevision() object literal, not map order.
 value:=struct{
 ID string `json:"id"`; Provider string `json:"provider"`; Protocol string `json:"protocol"`; Endpoint string `json:"endpoint"`; ModelName string `json:"modelName"`; Temperature float64 `json:"temperature"`; MaxTokens int64 `json:"maxTokens"`; Capabilities json.RawMessage `json:"capabilities"`; ReasoningOverride string `json:"reasoningOverride"`; ReasoningMapping json.RawMessage `json:"reasoningMapping,omitempty"`; Purposes []string `json:"purposes"`; EmbeddingOptions json.RawMessage `json:"embeddingOptions"`
 }{p.ID,p.Provider,p.Protocol,endpoint,p.ModelName,p.Temperature,p.MaxTokens,caps,override,p.ReasoningMapping,p.Purposes,embedding}
 result:=hashJSON(value);if result==""{return "",failure("MODEL_PROFILE_INVALID")};return result,nil
}

// admittedCapacity deliberately implements a finite catalog subset. Other
// official families are rejected, not silently downgraded to unknown reasoning.
// Unknown compatible endpoints have no verified features or capacity facts.
func admittedCapacity(p Profile)(contextTokens,outputTokens int64,err error){
 if p.Protocol!="openai"&&p.Protocol!="gemini"||present(p.ReasoningMapping){return 0,0,failure("GENERATION_MODEL_POLICY_UNSUPPORTED")}
 endpoint,err:=normalizedEndpoint(p.BaseURL);if err!=nil{return 0,0,err};u,_:=url.Parse(endpoint)
 switch u.Hostname(){
 case "api.openai.com":
  if p.Protocol=="openai"&&(endpoint=="https://api.openai.com"||endpoint=="https://api.openai.com/v1")&&(strings.TrimSpace(p.ModelName)=="gpt-4.1"||strings.TrimSpace(p.ModelName)=="gpt-4.1-2025-04-14"){return 1047576,32768,nil}
  return 0,0,failure("GENERATION_MODEL_POLICY_UNSUPPORTED")
 case "api.deepseek.com","generativelanguage.googleapis.com","api.siliconflow.cn","api.siliconflow.com","api.moonshot.cn","api.moonshot.ai","api.anthropic.com","openrouter.ai","opencode.ai","api.x.ai","text.novelai.net","open.bigmodel.cn":
  return 0,0,failure("GENERATION_MODEL_POLICY_UNSUPPORTED")
 }
 return 0,0,nil
}
func hints(p Profile) capabilityHints {var h capabilityHints;_ = json.Unmarshal(p.Capabilities,&h);if h.Context!=nil&&!positive(*h.Context){h.Context=nil};if !positive(h.Output){h.Output=0};return h}
func CreateReceipt(p Profile,id string,created,expires int64)(Receipt,error){
 if _,_,err:=admittedCapacity(p);err!=nil{return Receipt{},err};s,err:=subject(p);if err!=nil{return Receipt{},err};revision,err:=modelRevision(p);if err!=nil{return Receipt{},err}
 h:=hints(p);output:=int64(0);if positive(p.MaxTokens){output=p.MaxTokens};if h.Output>0{if output>0{output=min(output,h.Output)}else{output=h.Output}};if h.Context!=nil&&output>0{output=min(output,*h.Context)};if output==0{return Receipt{},failure("MODEL_OUTPUT_LIMIT_INVALID")}
 source:=EvidenceSource{Context:"unknown",Output:"legacy-profile",Features:"unknown"};if h.Context!=nil{source.Context="user-operational-cap"};if h.Output>0{source.Output="user-operational-cap"}
 evidence:=Evidence{Source:source,SubjectFingerprint:hashJSON(struct{Provider string `json:"provider"`;Protocol string `json:"protocol"`;Endpoint string `json:"endpoint"`;ModelName string `json:"modelName"`}{s.Provider,s.Protocol,s.Endpoint,p.ModelName}),Context:h.Context,Output:output}
 return Receipt{id,p.ID,p.Provider,p.Protocol,p.ModelName,revision,hashJSON(s),evidence,created,expires},nil
}

type RegistryOptions struct {
 LoadModel func(context.Context,string)(Profile,bool,error)
 Current func(context.Context,Scope) error
 // AuthorityLock holds the server settings revision and project/session gates.
 // Settings writes and project/tab close MUST acquire this same lock. Dispatch
 // retains it through egress.Do (response headers), never through body streaming.
 // It must not depend on a browser-supplied lease or no-op RPC callback.
 AuthorityLock func(context.Context,Scope)(func(),error)
 Now func() time.Time
 TTL time.Duration
}
type leaseRecord struct { receipt Receipt; profile Profile; scope Scope; purpose string; credentialHash string }
type Registry struct { mu sync.Mutex; options RegistryOptions; records map[string]leaseRecord }
func NewRegistry(options RegistryOptions)(*Registry,error){if options.LoadModel==nil||options.Current==nil{return nil,failure("MODEL_EXECUTION_CONFIG_REQUIRED")};if options.Now==nil{options.Now=time.Now};if options.TTL==0{options.TTL=4*time.Hour};if options.TTL<time.Millisecond||options.TTL>4*time.Hour{return nil,failure("MODEL_EXECUTION_CONFIG_INVALID")};return &Registry{options:options,records:map[string]leaseRecord{}},nil}
func (r *Registry) Begin(ctx context.Context,scope Scope,modelID,purpose string)(Receipt,error){
 if r==nil||!scope.valid()||purpose!="generation"{return Receipt{},failure("MODEL_EXECUTION_LEASE_INVALID")};if ctx.Err()!=nil{return Receipt{},ctx.Err()};if r.options.Current(ctx,scope)!=nil{return Receipt{},failure("GENERATION_EPOCH_STALE")}
 p,found,err:=r.options.LoadModel(ctx,modelID);if err!=nil{return Receipt{},failure("MODEL_PROFILE_UNAVAILABLE")};if !found||p.ID!=modelID{return Receipt{},failure("MODEL_NOT_FOUND")};p=cloneProfile(p);allowed:=false;for _,v:=range p.Purposes{if v==purpose{allowed=true}};if !allowed{return Receipt{},failure("MODEL_PURPOSE_INVALID")}
 var random [32]byte;if _,err:=rand.Read(random[:]);err!=nil{return Receipt{},failure("MODEL_EXECUTION_RANDOM_FAILED")};now:=r.options.Now().UnixMilli();receipt,err:=CreateReceipt(p,hex.EncodeToString(random[:]),now,now+r.options.TTL.Milliseconds());if err!=nil{return Receipt{},err}
 r.mu.Lock();defer r.mu.Unlock();for id,old:=range r.records{if now>=old.receipt.ExpiresAt{delete(r.records,id)}};if len(r.records)>=4096{return Receipt{},failure("MODEL_EXECUTION_LEASE_LIMIT")};r.records[receipt.LeaseID]=leaseRecord{receipt,p,scope,purpose,hashBytes(p.EncryptedCredential)};return clone(receipt),nil
}
func (r *Registry) resolve(ctx context.Context,id string,scope Scope,purpose string)(leaseRecord,error){
 if r==nil||!scope.valid(){return leaseRecord{},failure("MODEL_EXECUTION_LEASE_INVALID")};r.mu.Lock();record,ok:=r.records[id];if ok&&r.options.Now().UnixMilli()>=record.receipt.ExpiresAt{delete(r.records,id);r.mu.Unlock();return leaseRecord{},failure("MODEL_EXECUTION_LEASE_EXPIRED")};r.mu.Unlock()
 if !ok||scope!=record.scope||purpose!=record.purpose{return leaseRecord{},failure("MODEL_EXECUTION_LEASE_INVALID")};if ctx.Err()!=nil{return leaseRecord{},ctx.Err()};if r.options.Current(ctx,scope)!=nil{return leaseRecord{},failure("GENERATION_EPOCH_STALE")}
 current,found,err:=r.options.LoadModel(ctx,record.profile.ID);if err!=nil{return leaseRecord{},failure("MODEL_PROFILE_UNAVAILABLE")};revision,revisionErr:=modelRevision(current)
 if !found||revisionErr!=nil||revision!=record.receipt.ModelRevision||current.BaseURL!=record.profile.BaseURL||hashBytes(current.EncryptedCredential)!=record.credentialHash{r.mu.Lock();delete(r.records,id);r.mu.Unlock();return leaseRecord{},failure("GENERATION_MODEL_CHANGED")}
 return record,nil
}
// Resolve returns only a detached, secret-free receipt, never a model or key.
// resolveReproduce is resolve minus the live-session currency check. It is the record
// read ReproduceBinding uses, for the reason documented there: the run's own epoch is
// what a recovery reproduction asks about, and the live lease was proved at admission.
func (r *Registry) resolveReproduce(ctx context.Context,id string,scope Scope,purpose string)(leaseRecord,error){
 if r==nil||!scope.valid(){return leaseRecord{},failure("MODEL_EXECUTION_LEASE_INVALID")};r.mu.Lock();record,ok:=r.records[id];if ok&&r.options.Now().UnixMilli()>=record.receipt.ExpiresAt{delete(r.records,id);r.mu.Unlock();return leaseRecord{},failure("MODEL_EXECUTION_LEASE_EXPIRED")};r.mu.Unlock()
 if !ok||scope!=record.scope||purpose!=record.purpose{return leaseRecord{},failure("MODEL_EXECUTION_LEASE_INVALID")};if ctx.Err()!=nil{return leaseRecord{},ctx.Err()}
 current,found,err:=r.options.LoadModel(ctx,record.profile.ID);if err!=nil{return leaseRecord{},failure("MODEL_PROFILE_UNAVAILABLE")};revision,revisionErr:=modelRevision(current)
 if !found||revisionErr!=nil||revision!=record.receipt.ModelRevision||current.BaseURL!=record.profile.BaseURL||hashBytes(current.EncryptedCredential)!=record.credentialHash{r.mu.Lock();delete(r.records,id);r.mu.Unlock();return leaseRecord{},failure("GENERATION_MODEL_CHANGED")}
 return record,nil
}
func (r *Registry) Resolve(ctx context.Context,id string,scope Scope,purpose string)(Receipt,error){record,err:=r.resolve(ctx,id,scope,purpose);return clone(record.receipt),err}
func (r *Registry) Close(ctx context.Context,id string,scope Scope,purpose string) bool {if _,err:=r.resolve(ctx,id,scope,purpose);err!=nil{return false};r.mu.Lock();defer r.mu.Unlock();delete(r.records,id);return true}
// RevokeScope is called by server session/project close, never browser lease text.
func (r *Registry) RevokeScope(scope Scope){if r==nil{return};r.mu.Lock();defer r.mu.Unlock();for id,record:=range r.records{if record.scope==scope{delete(r.records,id)}}}
