package generationowner

import (
 "context"
 "encoding/json"
 "regexp"
 "sort"
 "strings"
 "unicode/utf8"
)

// SourceKey is a finite typed capability selection, never a file/SQL/URL locator.
// These foundations do not grant repository authority: the embedding server
// explicitly supplies each resolver for a captured admitted ordinary project.
// No resolver means unsupported, never a browser-content fallback.
type SourceKey struct { Kind string `json:"kind"`; ID string `json:"id"` }
type SourceValue struct { Revision int64; Text string }
type SourceReader func(context.Context,Scope,SourceKey)(SourceValue,error)
type Sources struct {
 ProjectCore SourceReader
 Blueprint SourceReader
 Draft SourceReader
 Prompt SourceReader
}
type Ref struct {
 ProjectID string `json:"projectId"`
 Epoch string `json:"epoch"`
 SourceID string `json:"sourceId"`
 Revision int64 `json:"revision"`
 ContentHash string `json:"contentHash"`
}
type material struct { key SourceKey; ref Ref; text string; read SourceReader }
type Binding struct { registry *Registry; scope Scope; modelRevision string; materials []material; digest string }
var numericID=regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
var promptID=regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}:(zh-CN|en-US)$`)
func (s Sources) reader(key SourceKey)(SourceReader,string,error){
 switch key.Kind {
 case "project-core":if key.ID=="main"&&s.ProjectCore!=nil{return s.ProjectCore,"project-core:main",nil}
 case "blueprint":if numericID.MatchString(key.ID)&&s.Blueprint!=nil{return s.Blueprint,"blueprint:"+key.ID,nil}
 case "draft":if numericID.MatchString(key.ID)&&s.Draft!=nil{return s.Draft,"draft:"+key.ID,nil}
 case "prompt":if promptID.MatchString(key.ID)&&s.Prompt!=nil{return s.Prompt,"prompt:"+key.ID,nil}
 }
 return nil,"",failure("GENERATION_SOURCE_UNSUPPORTED")
}
// BindSources only freezes explicit capability reads and their exact UTF-8
// bytes/revisions. This is not generation-source-binding.ts's full RunBinding:
// knowledge/finalized/portable/skills/MCP/author-action/graph/planning refs and
// material decisions remain unsupported. It cannot substitute for owner task
// construction or assert that arbitrary renderer messages use these materials.
func (r *Registry) BindSources(ctx context.Context,scope Scope,leaseID string,sources Sources,selection []SourceKey)(*Binding,error){
 record,err:=r.resolve(ctx,leaseID,scope,"generation");if err!=nil{return nil,err};if len(selection)==0||len(selection)>256{return nil,failure("GENERATION_BINDING_INVALID")};keys:=append([]SourceKey(nil),selection...);sort.Slice(keys,func(i,j int)bool{if keys[i].Kind!=keys[j].Kind{return keys[i].Kind<keys[j].Kind};return keys[i].ID<keys[j].ID})
 binding:=&Binding{registry:r,scope:scope,modelRevision:record.receipt.ModelRevision};total:=0
 for i,key:=range keys{if i>0&&key==keys[i-1]{return nil,failure("GENERATION_BINDING_INVALID")};read,id,err:=sources.reader(key);if err!=nil{return nil,err};value,err:=read(ctx,scope,key);if err!=nil{return nil,failure("GENERATION_SOURCE_UNAVAILABLE")};if value.Revision<0||value.Revision>safeInteger||!utf8.ValidString(value.Text){return nil,failure("GENERATION_BINDING_INVALID")};total+=len(value.Text);if total>8*1024*1024{return nil,failure("GENERATION_SOURCE_TOO_LARGE")};binding.materials=append(binding.materials,material{key,Ref{scope.Project,scope.Epoch,id,value.Revision,hashBytes([]byte(value.Text))},strings.Clone(value.Text),read})}
 if err:=binding.validate(ctx);err!=nil{return nil,err};binding.digest=hashJSON(struct{Project string `json:"projectId"`;Epoch string `json:"epoch"`;Model string `json:"modelRevision"`;Sources []Ref `json:"sources"`}{scope.Project,scope.Epoch,binding.modelRevision,binding.Refs()});return binding,nil
}
func(b *Binding) Refs()[]Ref{if b==nil{return nil};out:=make([]Ref,len(b.materials));for i,m:=range b.materials{out[i]=m.ref};return out}
func(b *Binding) Fingerprint()string{if b==nil{return ""};return b.digest}
func(b *Binding) validate(ctx context.Context)error{
 if b==nil||b.registry==nil{return failure("GENERATION_BINDING_INVALID")};if b.registry.options.Current(ctx,b.scope)!=nil{return failure("GENERATION_EPOCH_STALE")};for _,m:=range b.materials{value,err:=m.read(ctx,b.scope,m.key);if err!=nil{return failure("GENERATION_SOURCE_UNAVAILABLE")};if value.Revision!=m.ref.Revision||value.Text!=m.text{return failure("GENERATION_SOURCE_CHANGED")}};return nil
}
// Metadata is deliberately minimal and secret-free; full durable RunBinding
// compatibility is NOT claimed by this foundation.
func(b *Binding) Metadata()json.RawMessage{if b==nil{return nil};raw,_:=json.Marshal(struct{Fingerprint string `json:"fingerprint"`;Sources []Ref `json:"sources"`}{b.digest,b.Refs()});return raw}
