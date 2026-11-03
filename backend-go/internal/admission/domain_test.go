package admission

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)

func TestTypeScriptAuthorContentHash(t *testing.T){
 path:=os.Getenv("GO_SQLITE_CONTRACT");if path==""{t.Skip("GitHub TypeScript oracle")}
 file,err:=os.Open(path);if err!=nil{t.Fatal(err)};contract,err:=ParseContract(file);file.Close();if err!=nil{t.Fatal(err)}
 body,err:=os.ReadFile(filepath.Join(filepath.Dir(path),"domain-proofs.json"));if err!=nil{t.Fatal(err)}
 var fixtures []struct{File string `json:"file"`;Fingerprint string `json:"fingerprint"`;Domain DomainEvidence `json:"domain"`}
 if err:=json.Unmarshal(body,&fixtures);err!=nil{t.Fatal(err)};if len(fixtures)!=7{t.Fatal("missing logical proof oracles")}
 for _,fixture:=range fixtures{t.Run(fixture.File,func(t *testing.T){
  expected:=contract;if fixture.Fingerprint!=""{expected=SchemaContract{{Version:7,Fingerprints:[]string{fixture.Fingerprint}}}}
  evidence,err:=VerifySnapshotWithDomain(context.Background(),filepath.Join(filepath.Dir(path),fixture.File),expected);if err!=nil{t.Fatal(err)}
  if evidence.Domain==nil||!reflect.DeepEqual(*evidence.Domain,fixture.Domain){t.Fatalf("logical author proof differs from TypeScript: got %+v want %+v",evidence.Domain,fixture.Domain)}
 })}
}
func TestJSStorageClassesAndUTF16Order(t *testing.T){
 if !lessUTF16("𐀀","\ue000")||lessUTF16("\ue000","𐀀"){t.Fatal("must sort UTF16 units, not Unicode scalar values")}
 for _,test:=range []struct{value any;expected string}{
  {nil,`null`},{int64(-9223372036854775808),`{"integer":"-9223372036854775808"}`},{float64(1e-7),`1e-7`},{float64(1e21),`1e+21`},{float64(-0),`0`},
  {[]byte{},`{"type":"Buffer","data":[]}`},{[]byte{0,255},`{"type":"Buffer","data":[0,255]}`},
 }{encoded,err:=appendSQLiteValue(nil,test.value);if err!=nil||string(encoded)!=test.expected{t.Fatal(string(encoded),err,test.expected)}}
}
func TestDomainProofIsOptInAndCountsOnlyNonemptyTables(t *testing.T){
 path,db:=fixtureDB(t);db.Close()
 evidence,err:=VerifySnapshot(context.Background(),path,syntheticContract());if err!=nil||evidence.Domain!=nil{t.Fatal(evidence,err)}
 evidence,err=VerifySnapshotWithDomain(context.Background(),path,syntheticContract());if err!=nil||evidence.Domain==nil||evidence.Domain.TableCounts["children"]!=1{t.Fatal(evidence,err)}
}
