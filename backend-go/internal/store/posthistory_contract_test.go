package store

import (
 "bytes"
 "context"
 "database/sql"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)
func postHistoryContractValue(value any)any{
 switch value:=value.(type){case map[string]any:if value==nil{return nil};out:=map[string]any{};for key,item:=range value{if key=="created_at"||key=="updated_at"||key=="createdAt"||key=="updatedAt"{continue};if key=="completed_at"||key=="last_attempt_at"||key=="completedAt"||key=="lastAttemptAt"{if text,ok:=item.(string);ok&&text!=""{out[key]="<timestamp>";continue}};out[key]=postHistoryContractValue(item)};return out;case []map[string]any:out:=make([]any,len(value));for i,item:=range value{out[i]=postHistoryContractValue(item)};return out;case []any:out:=make([]any,len(value));for i,item:=range value{out[i]=postHistoryContractValue(item)};return out;default:return reviewRevisionContractValue(value)}
}
func TestOriginalPostProcessAndHistoryRepositoriesContracts(t *testing.T){
 path:=os.Getenv("GO_POSTHISTORY_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original postprocess/history oracle mandatory")};t.Skip("GitHub-only original repository oracle")};data,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
 var oracle struct{FormatVersion int `json:"formatVersion"`;Cases []struct{Name string `json:"name"`;Database string `json:"database"`;Method string `json:"method"`;Input map[string]any `json:"input"`;Args []any `json:"args"`;Result any `json:"result"`;Error string `json:"error"`;After map[string]any `json:"after"`} `json:"cases"`};if err:=json.Unmarshal(data,&oracle);err!=nil{t.Fatal(err)};if oracle.FormatVersion!=1||len(oracle.Cases)<60{t.Fatal("incomplete original post/history oracle",len(oracle.Cases))}
 normalize:=func(value any)any{encoded,err:=json.Marshal(postHistoryContractValue(value));if err!=nil{t.Fatal(err)};var parsed any;if err:=json.Unmarshal(encoded,&parsed);err!=nil{t.Fatal(err)};return parsed}
 for _,fixture:=range oracle.Cases{t.Run(fixture.Name,func(t *testing.T){
  original,err:=os.ReadFile(fixture.Database);if err!=nil{t.Fatal(err)};copy:=filepath.Join(t.TempDir(),"copy.db");if err:=os.WriteFile(copy,original,0600);err!=nil{t.Fatal(err)};dsn,err:=ExistingDSN(copy);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer db.Close();repository:=NewPostHistory(db);repository.newRunID=func()(string,error){return "11111111-1111-4111-8111-111111111111",nil};ctx:=context.Background();var result any;var opErr error
  switch fixture.Method{
  case "CreateRun":var id string;id,opErr=repository.CreateRun(ctx,fixture.Input);if opErr==nil{result=id}
  case "LatestRun":result,opErr=repository.LatestRun(ctx,fixture.Args[0].(string),fixture.Args[1].(string))
  case "Steps":result,opErr=repository.Steps(ctx,fixture.Args[0].(string))
  case "MarkStepOK":opErr=repository.MarkStepOK(ctx,fixture.Args[0].(string),fixture.Args[1].(string))
  case "MarkStepFailed":opErr=repository.MarkStepFailed(ctx,fixture.Args[0].(string),fixture.Args[1].(string),fixture.Args[2].(string))
  case "AllPassed":result,opErr=repository.AllPassed(ctx,fixture.Args[0].(string),fixture.Args[1].(string))
  case "FailedStepLabels":result,opErr=repository.FailedStepLabels(ctx,fixture.Args[0].(string))
  case "LogCall":opErr=repository.LogCall(ctx,fixture.Input)
  case "Stats":result,opErr=repository.Stats(ctx)
  case "History":limit:=int64(50);if len(fixture.Args)>0&&fixture.Args[0]!=nil{limit=int64(fixture.Args[0].(float64))};result,opErr=repository.History(ctx,limit)
  default:t.Fatal("unknown original method",fixture.Method)
  }
  code:="";if opErr!=nil{code=opErr.Error()};if code!=fixture.Error{t.Fatal("error Go/TS",code,fixture.Error)};if !reflect.DeepEqual(normalize(result),normalize(fixture.Result)){t.Fatalf("result Go=%#v TS=%#v",normalize(result),normalize(fixture.Result))}
  after:=map[string]any{};for table:=range fixture.After{if table!="post_process_runs"&&table!="post_process_steps"&&table!="llm_calls"{t.Fatal("oracle unexpected table",table)};rows,err:=db.QueryContext(ctx,"SELECT * FROM "+table+" ORDER BY rowid");if err!=nil{t.Fatal(err)};columns,err:=rows.Columns();if err!=nil{rows.Close();t.Fatal(err)};records:=[]any{};for rows.Next(){values:=make([]any,len(columns));pointers:=make([]any,len(columns));for i:=range values{pointers[i]=&values[i]};if err:=rows.Scan(pointers...);err!=nil{rows.Close();t.Fatal(err)};record:=map[string]any{};for i,key:=range columns{record[key]=values[i]};records=append(records,record)};err=rows.Err();closeErr:=rows.Close();if err!=nil||closeErr!=nil{t.Fatal(err,closeErr)};after[table]=records}
  if !reflect.DeepEqual(normalize(after),normalize(fixture.After)){t.Fatalf("database effects differ\nGo=%#v\nTS=%#v",normalize(after),normalize(fixture.After))};still,err:=os.ReadFile(fixture.Database);if err!=nil||!bytes.Equal(still,original){t.Fatal("original fixture mutated",err)}
 })}
}
