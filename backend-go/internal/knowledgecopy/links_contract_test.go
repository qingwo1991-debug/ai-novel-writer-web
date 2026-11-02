package knowledgecopy

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "os"
 "path/filepath"
 "testing"
)
func TestLinksOriginalTypeScriptCurrentPredicate(t *testing.T){path:=os.Getenv("GO_KNOWLEDGECOPY_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original oracle mandatory")};t.Skip("GitHub original oracle required")};raw,err:=os.ReadFile(path);if err!=nil{t.Fatal(err)};var contract struct{LinkCases []struct{Name,ID,FilePath string;Raw *string;Managed,Current bool} `json:"linkCases"`};if err:=json.Unmarshal(raw,&contract);err!=nil||len(contract.LinkCases)!=9{t.Fatal("invalid link contract",err)};for _,item:=range contract.LinkCases{t.Run(item.Name,func(t *testing.T){dir:=t.TempDir();if err:=os.Chmod(dir,0700);err!=nil{t.Fatal(err)};if item.Raw!=nil{copies:=filepath.Join(dir,"knowledge-copies");if err:=os.Mkdir(copies,0700);err!=nil{t.Fatal(err)};hash:=sha256.Sum256([]byte(item.ID));if err:=os.WriteFile(filepath.Join(copies,hex.EncodeToString(hash[:])+".json"),[]byte(*item.Raw),0600);err!=nil{t.Fatal(err)}};root,err:=os.OpenRoot(dir);if err!=nil{t.Fatal(err)};defer root.Close();got,err:=ValidateLinks(context.Background(),root,[]DocumentRef{{ID:item.ID,FilePath:item.FilePath}},Budget{MaxMetadataBytes:1024*1024,MaxCopies:100});if err!=nil{t.Fatal(err)};expectedCurrent:=0;if item.Managed&&item.Current{expectedCurrent=1};if got.Documents!=1||got.Managed!=boolInt(item.Managed)||got.Current!=expectedCurrent{t.Fatal("original predicate mismatch",got)};if !item.Managed&&got.Unreferenced!=boolInt(item.Raw!=nil){t.Fatal("orphan copy discarded",got)}})}}
func boolInt(value bool)int{if value{return 1};return 0}
