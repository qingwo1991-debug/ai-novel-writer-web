package llm

import (
 "bytes"
 "encoding/json"
)
// Preserve the JS undefined-vs-null wire distinction for fixture/policy input.
// Public generation policy normally supplies a number or omits temperature.
func(m *Model)UnmarshalJSON(data []byte)error{
 type alias Model;var decoded alias;if err:=json.Unmarshal(data,&decoded);err!=nil{return err};var fields map[string]json.RawMessage;if err:=json.Unmarshal(data,&fields);err!=nil{return err};_,exists:=fields["maxTokens"];decoded.MaxTokensMissing=!exists;*m=Model(decoded);return nil
}
func(r *Reasoning)UnmarshalJSON(data []byte)error{
 type alias Reasoning;var decoded alias;if err:=json.Unmarshal(data,&decoded);err!=nil{return err};var fields map[string]json.RawMessage;if err:=json.Unmarshal(data,&fields);err!=nil{return err};_,exists:=fields["thinkingBudget"];decoded.ThinkingBudgetMissing=!exists;*r=Reasoning(decoded);return nil
}
func(o *Options)UnmarshalJSON(data []byte)error{
 type alias Options
 var decoded alias;if err:=json.Unmarshal(data,&decoded);err!=nil{return err}
 var fields map[string]json.RawMessage;if err:=json.Unmarshal(data,&fields);err!=nil{return err}
 if raw,exists:=fields["temperature"];exists&&bytes.Equal(bytes.TrimSpace(raw),[]byte("null")){decoded.TemperatureNull=true}
 *o=Options(decoded);return nil
}
