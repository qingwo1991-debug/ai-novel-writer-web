package transport

import (
 "context"
 "encoding/json"
 "net/http"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
 "errors"
 "strings"
 "testing"
)
func TestReviewRevisionBindingPreflightAndInputBeforeProvider(t *testing.T){
 server:=fixture(t);providerCalls:=0;provider:=ReviewRevisionProviderFunc(func(context.Context,Caller,any)(*store.ReviewRevisions,func(),error){providerCalls++;return nil,nil,errors.New("private source /secret")})
 if err:=server.Bind("db:revision-get-full",func(context.Context,Caller,[]any)(any,error){return "existing",nil});err!=nil{t.Fatal(err)}
 if err:=server.BindReviewRevisions(provider);err==nil{t.Fatal("partial binding accepted")};if _,ok:=server.handlers["db:review-create"];ok{t.Fatal("failed binding partially registered operations")}
 server=fixture(t);if err:=server.BindReviewRevisions(provider);err!=nil{t.Fatal(err)};cookie,csrf:=bootstrap(t,server)
 expected:=draftContext("synthetic-project","synthetic-lease","/synthetic/book");request:=map[string]any{"baseDraftId":float64(1),"content":"report","reviewCycleId":nil}
 response:=draftRPC(t,server,cookie,csrf,"synthetic-tab-00001","db:review-create",request,"/synthetic/book",expected);if response.Code!=http.StatusOK||!strings.Contains(response.Body.String(),"REVIEW_CYCLE_OWNER_REQUIRED"){t.Fatal(response.Code,response.Body.String())};if providerCalls!=0{t.Fatal("unowned mutation reached provider")}
 delete(request,"reviewCycleId");response=draftRPC(t,server,cookie,csrf,"synthetic-tab-00001","db:review-create",request,"/synthetic/book",expected);if strings.Contains(response.Body.String(),"/secret")||!strings.Contains(response.Body.String(),"REVIEW_REVISION_OPERATION_FAILED"){t.Fatal(response.Body.String())};if providerCalls!=1{t.Fatal(providerCalls)}
}
func TestReviewRevisionValidationAndDiagnostics(t *testing.T){
 valid:=map[string]any{"baseDraftId":float64(1),"revisionType":"refine","content":"body","wordCount":float64(3)}
 if err:=validateReviewRevisionInput("db:revision-create",valid);err!=nil{t.Fatal("missing snapshot must be domain failure",err)}
 for _,key:=range []string{"reviewCycleId","generationRunHandle","authorRecovery"}{candidate:=map[string]any{};for k,v:=range valid{candidate[k]=v};candidate[key]=nil;if err:=validateReviewRevisionInput("db:revision-create",candidate);reviewRevisionCode(err)!="REVIEW_CYCLE_OWNER_REQUIRED"{t.Fatal(key,err)}}
 valid["expectedSource"]=map[string]any{"id":float64(1),"chapterNumber":float64(1),"version":float64(2),"status":"finalized","content":"exact source"};if err:=validateReviewRevisionInput("db:revision-create",valid);err!=nil{t.Fatal("matching noneditable source can be inspected",err)}
 valid["expectedSource"]=map[string]any{"id":float64(1)};if err:=validateReviewRevisionInput("db:revision-create",valid);reviewRevisionCode(err)!="SOURCE_DRAFT_CHANGED"{t.Fatal("partial baseline lost source conflict",err)}
 if got:=reviewRevisionCode(errors.New("sqlite error: private prompt path /secret"));got!="REVIEW_REVISION_OPERATION_FAILED"{t.Fatal(got)}
 encoded,err:=json.Marshal(reviewRevisionWire(map[string]any{"sourceDraft":nil,"content":[]byte{0,255}}));if err!=nil{t.Fatal(err)};if !strings.Contains(string(encoded),`"content":{"data":[0,255],"type":"Buffer"}`)||!strings.Contains(string(encoded),`"sourceDraft":null`){t.Fatal(string(encoded))}
 for _,input:=range []map[string]any{{"revisionId":float64(1),"targetDraftId":float64(1),"expectedDraftContent":"old","mergedContent":"new","wordCount":float64(-1)},{"revisionId":float64(1),"targetDraftId":float64(1),"expectedDraftContent":nil,"mergedContent":"new","wordCount":float64(1)}}{if err:=validateReviewRevisionInput("db:revision-merge",input);err==nil{t.Fatal("malformed merge passed",input)}}
}
