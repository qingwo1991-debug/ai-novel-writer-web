package admission

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "reflect"
 "testing"
)
func TestTypeScriptDomainProofBeyondOld64MiBCeiling(t *testing.T){
 contract:=os.Getenv("GO_SQLITE_CONTRACT");if contract==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original oracle required")};t.Skip("GitHub synthetic large-row oracle")}
 body,err:=os.ReadFile(filepath.Join(filepath.Dir(contract),"large-domain-proof.json"));if err!=nil{t.Fatal(err)}
 var fixture struct{File string `json:"file"`;Fingerprint string `json:"fingerprint"`;Domain DomainEvidence `json:"domain"`};if err:=json.Unmarshal(body,&fixture);err!=nil{t.Fatal(err)}
 path:=filepath.Join(filepath.Dir(contract),fixture.File);before:=hashFile(t,path)
 evidence,err:=VerifySnapshotWithDomain(context.Background(),path,SchemaContract{{Version:7,Fingerprints:[]string{fixture.Fingerprint}}});if err!=nil{t.Fatal(err)}
 if evidence.Domain==nil||!reflect.DeepEqual(*evidence.Domain,fixture.Domain)||fixture.Domain.TableCounts["large_rows"]!=280{t.Fatal("external proof differs from Node",evidence.Domain,fixture.Domain)}
 if hashFile(t,path)!=before{t.Fatal("logical proof wrote source fixture")}
}
