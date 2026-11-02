package store

import (
 "bytes"
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)
func TestOriginalPlotTreeRepositoryContracts(t *testing.T){
 path:=os.Getenv("GO_PLOTTREE_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original plot tree repository oracle mandatory")};t.Skip("GitHub-only original repository oracle")};data,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)}
 var oracle struct{FormatVersion int `json:"formatVersion"`;Cases []struct{Name string `json:"name"`;Database string `json:"database"`;Method string `json:"method"`;Input map[string]any `json:"input"`;ExpectedSourceRevision string `json:"expectedSourceRevision"`;Result any `json:"result"`;Error string `json:"error"`;After map[string]any `json:"after"`} `json:"cases"`};if err:=json.Unmarshal(data,&oracle);err!=nil{t.Fatal(err)};if oracle.FormatVersion!=1||len(oracle.Cases)<50{t.Fatal("incomplete original plot repository oracle",len(oracle.Cases))}
 normalize:=func(value any)any{encoded,err:=json.Marshal(reviewRevisionContractValue(value));if err!=nil{t.Fatal(err)};var parsed any;if err:=json.Unmarshal(encoded,&parsed);err!=nil{t.Fatal(err)};return parsed}
 for _,fixture:=range oracle.Cases{t.Run(fixture.Name,func(t *testing.T){
  original,err:=os.ReadFile(fixture.Database);if err!=nil{t.Fatal(err)};copy:=filepath.Join(t.TempDir(),"copy.db");if err:=os.WriteFile(copy,original,0600);err!=nil{t.Fatal(err)};dsn,err:=ExistingDSN(copy);if err!=nil{t.Fatal(err)};db,err:=sql.Open("sqlite3",dsn);if err!=nil{t.Fatal(err)};defer db.Close();repository:=NewPlotTree(db);ctx:=context.Background();var result any;var opErr error
  switch fixture.Method{case "Read":result,opErr=repository.Read(ctx);case "Save":result,opErr=repository.Save(ctx,fixture.Input,fixture.ExpectedSourceRevision);case "Clear":opErr=repository.Clear(ctx);default:t.Fatal("unknown plot method",fixture.Method)}
  code:="";if opErr!=nil{code=opErr.Error()};if code!=fixture.Error{t.Fatal("error Go/TS",code,fixture.Error)};if !reflect.DeepEqual(normalize(result),normalize(fixture.Result)){t.Fatalf("result Go=%#v TS=%#v",normalize(result),normalize(fixture.Result))}
  after:=map[string]any{};for table:=range fixture.After{if table!="project_core"{t.Fatal("unexpected plot effect table",table)};records,err:=rrMaps(ctx,db,"SELECT * FROM project_core ORDER BY rowid");if err!=nil{t.Fatal(err)};after[table]=records};if !reflect.DeepEqual(normalize(after),normalize(fixture.After)){t.Fatalf("database effects differ\nGo=%#v\nTS=%#v",normalize(after),normalize(fixture.After))};still,err:=os.ReadFile(fixture.Database);if err!=nil||!bytes.Equal(still,original){t.Fatal("original fixture mutated",err)}
 })}
}
func TestPlotTreeJavaScriptFactSerialization(t *testing.T){
 text,err:=plotJSON(plotObject{{"writingLanguage","zh-CN"},{"synopsis",plotObject{{"content","中文😀<&>\u2028\u2029\x00\b\f\n\r\t"}}},{"blueprints",[]any{plotObject{{"chapterNumber",1e-7},{"title",[]byte{0,255}},{"purpose",nil},{"keyEvents","x"}}}},{"finalizedChapters",[]any{}},{"narrativeThreads",[]any{}}});if err!=nil{t.Fatal(err)}
 expected:="{\"writingLanguage\":\"zh-CN\",\"synopsis\":{\"content\":\"中文😀<&>\u2028\u2029\\u0000\\b\\f\\n\\r\\t\"},\"blueprints\":[{\"chapterNumber\":1e-7,\"title\":{\"type\":\"Buffer\",\"data\":[0,255]},\"purpose\":null,\"keyEvents\":\"x\"}],\"finalizedChapters\":[],\"narrativeThreads\":[]}"
 if text!=expected{t.Fatal("not original ordered JSON.stringify bytes",text,expected)};hash:=sha256.Sum256([]byte(text));if len(hex.EncodeToString(hash[:]))!=64{t.Fatal("hash")}
}
func TestPlotTreeNilDatabaseAndCorruptSnapshotIsolation(t *testing.T){
 ctx:=context.Background();for _,repository:=range []*PlotTree{nil,NewPlotTree(nil)}{if _,err:=repository.Read(ctx);err!=ErrProjectNotOpen{t.Fatal(err)};if _,err:=repository.Save(ctx,nil,"");err!=ErrProjectNotOpen{t.Fatal(err)};if err:=repository.Clear(ctx);err!=ErrProjectNotOpen{t.Fatal(err)}}
 db,_,_:=narrativeFixture(t);repository:=NewPlotTree(db);const corrupt=`{"version":1} trailing bad json`;mustExec(t,db,"INSERT INTO project_core(id,plot_tree_snapshot)VALUES('main',?)",corrupt);first,err:=repository.Read(ctx);if err!=nil||first["storedSnapshotInvalid"]!=true||first["snapshot"]!=nil{t.Fatal(first,err)};var raw string;if err:=db.QueryRow("SELECT plot_tree_snapshot FROM project_core WHERE id='main'").Scan(&raw);err!=nil||raw!=corrupt{t.Fatal("corrupt snapshot destroyed",raw,err)};if err:=repository.Clear(ctx);err!=nil{t.Fatal(err)};second,err:=repository.Read(ctx);if err!=nil||second["snapshot"]!=nil||second["storedSnapshotInvalid"]!=nil||second["sourceRevision"]!=first["sourceRevision"]{t.Fatal("snapshot altered source facts",first,second,err)}
}
