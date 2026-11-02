package store

import(
 "bytes"
 "context"
 "database/sql"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)
func TestOriginalBlueprintRepositoryContracts(t *testing.T){
 path:=os.Getenv("GO_BLUEPRINT_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original blueprint contract mandatory")};t.Skip("GitHub-only original blueprint oracle")};data,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)};var oracle struct{FormatVersion int `json:"formatVersion"`;Cases []struct{Name string `json:"name"`;Database string `json:"database"`;Method string `json:"method"`;Input any `json:"input"`;Args []any `json:"args"`;Result any `json:"result"`;Error string `json:"error"`;After map[string]any `json:"after"`} `json:"cases"`};if err:=json.Unmarshal(data,&oracle);err!=nil{t.Fatal(err)};if oracle.FormatVersion!=1||len(oracle.Cases)<100{t.Fatal("incomplete original blueprint oracle",len(oracle.Cases))}
 // notes_updated_at/notesUpdatedAt are author fields, NEVER ignored as clocks.
 var clean func(any)any;clean=func(value any)any{switch value:=value.(type){case []byte:return reviewRevisionContractValue(value);case []any:if value==nil{return nil};out:=make([]any,len(value));for i,v:=range value{out[i]=clean(v)};return out;case []map[string]any:if value==nil{return nil};out:=make([]any,len(value));for i,v:=range value{out[i]=clean(v)};return out;case map[string]any:if value==nil{return nil};out:=map[string]any{};for key,v:=range value{switch key{case "created_at","updated_at","completed_at","createdAt","updatedAt","completedAt":continue;case "characters","character_sync_input":if text,ok:=v.(string);ok{var parsed any;if json.Unmarshal([]byte(text),&parsed)==nil{out[key]=map[string]any{"parsedSerializedJSON":clean(parsed)};continue}}};out[key]=clean(v)};return out};return value}
 normalize:=func(value any)any{encoded,err:=json.Marshal(clean(value));if err!=nil{t.Fatal(err)};var parsed any;if err:=json.Unmarshal(encoded,&parsed);err!=nil{t.Fatal(err)};return parsed}
 for _,fixture:=range oracle.Cases{t.Run(fixture.Name,func(t *testing.T){original,err:=os.ReadFile(fixture.Database);if err!=nil{t.Fatal(err)};copy:=filepath.Join(t.TempDir(),"copy.db");if err:=os.WriteFile(copy,original,0600);err!=nil{t.Fatal(err)};dsn,err:=ExistingDSN(copy);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer db.Close();repository:=NewBlueprints(db);ctx:=context.Background();var result any;var opErr error
 var first any;if len(fixture.Args)>0{first=fixture.Args[0]};switch fixture.Method{case "All":result,opErr=repository.All(ctx);case "Get":result,opErr=repository.Get(ctx,first);case "Upsert":input,_:=fixture.Input.(map[string]any);opErr=repository.Upsert(ctx,input);case "UpsertMany":values,_:=fixture.Input.([]any);batch:=make([]map[string]any,len(values));for i,value:=range values{batch[i],_=value.(map[string]any)};opErr=repository.UpsertMany(ctx,batch);case "Delete":opErr=repository.Delete(ctx,first);case "CommitRange":input,_:=fixture.Input.(map[string]any);result,opErr=repository.CommitRange(ctx,input);case "PendingSync":result,opErr=repository.PendingSync(ctx);case "GetSync":id,_:=first.(string);result,opErr=repository.GetSync(ctx,id);default:t.Fatal("unknown blueprint method",fixture.Method)}
 code:="";if opErr!=nil{code=opErr.Error()};if code!=fixture.Error{t.Fatal("error Go/TS",code,fixture.Error)};if !reflect.DeepEqual(normalize(result),normalize(fixture.Result)){t.Fatalf("result Go=%#v TS=%#v",normalize(result),normalize(fixture.Result))};after:=map[string]any{};for table:=range fixture.After{switch table{case "blueprints","blueprint_commit_operations","blueprint_character_sync_operations":default:t.Fatal("unexpected effect table",table)};rows,err:=rrMaps(ctx,db,"SELECT * FROM "+table+" ORDER BY rowid");if err!=nil{t.Fatal(err)};after[table]=rows};if !reflect.DeepEqual(normalize(after),normalize(fixture.After)){t.Fatalf("effects Go=%#v TS=%#v",normalize(after),normalize(fixture.After))};still,err:=os.ReadFile(fixture.Database);if err!=nil||!bytes.Equal(still,original){t.Fatal("original fixture mutated",err)}
 })}
}
