// Package policy loads the existing hand-reviewed public operation inventory.
// Membership does not imply implementation or authorization.
package policy

import (
 "encoding/json"
 "errors"
 "io"
 "strings"
)

type Scope string
const(Global Scope="global"; Project Scope="project"; Flexible Scope="flexible")
type Registry struct{Version int `json:"version"`;Operations map[string]Scope `json:"operations"`;Excluded map[string]string `json:"excluded"`}
func Load(reader io.Reader)(*Registry,error){
 var registry Registry;decoder:=json.NewDecoder(io.LimitReader(reader,1024*1024));if err:=decoder.Decode(&registry);err!=nil{return nil,err}
 var extra any;if err:=decoder.Decode(&extra);err!=io.EOF{return nil,errors.New("OPERATION_POLICY_TRAILING_DATA")}
 if registry.Version!=1||len(registry.Operations)==0{return nil,errors.New("OPERATION_POLICY_INVALID")}
 for operation,scope:=range registry.Operations{
  if strings.HasPrefix(operation,"web:")||!strings.Contains(operation,":"){return nil,errors.New("OPERATION_POLICY_INTERNAL_EXPOSED")}
  if _,exists:=registry.Excluded[operation];exists{return nil,errors.New("OPERATION_POLICY_SCOPE_COLLISION")}
  if scope!=Global&&scope!=Project&&scope!=Flexible{return nil,errors.New("OPERATION_POLICY_SCOPE_INVALID")}
 }
 return &registry,nil
}
func(r *Registry)Lookup(operation string)(Scope,bool){scope,ok:=r.Operations[operation];return scope,ok}
