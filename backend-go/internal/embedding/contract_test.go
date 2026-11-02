package embedding

import (
 "bytes"
 "context"
 "net/http"
 "net/http/httptest"
 "strconv"
 "sync"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
 "encoding/json"
 "errors"
 "os"
 "reflect"
 "testing"
)

type embeddingOracle struct{
 Format int `json:"format"`
 Validators []struct{Name string;Protocol Protocol;BatchLength int;Response any;Expected [][]float64;Error string}
 Requests []struct{Name string;Protocol Protocol;Model struct{BaseURL string `json:"baseUrl"`;APIKey string `json:"apiKey"`;ModelName string `json:"modelName"`};Texts []string;Request struct{URL string;Method string;Headers map[string]string;Body any};Expected [][]float64;Error string}
 Batches []struct{Name string;Protocol Protocol;Size *int;TextCount int;BatchLengths []int;AuthorityChecks int;Expected [][]float64}
 Chunks []struct{Name,Text string;MaxChars,Overlap int;Expected []string;Error string;OriginalContainsLoneSurrogate bool}
}
func readEmbeddingOracle(t *testing.T)embeddingOracle{t.Helper();path:=os.Getenv("GO_EMBEDDING_CONTRACT");if path==""{if os.Getenv("GITHUB_ACTIONS")=="true"{t.Fatal("original embedding contract mandatory on GitHub")};t.Skip("GitHub-only original embedding contract")};file,err:=os.Open(path);if err!=nil{t.Fatal(err)};defer file.Close();decoder:=json.NewDecoder(file);decoder.UseNumber();var oracle embeddingOracle;if err:=decoder.Decode(&oracle);err!=nil{t.Fatal(err)};if oracle.Format!=1||len(oracle.Validators)!=38||len(oracle.Requests)!=16||len(oracle.Batches)!=12||len(oracle.Chunks)!=240{t.Fatal("incomplete original contract",oracle.Format,len(oracle.Validators),len(oracle.Requests),len(oracle.Batches),len(oracle.Chunks))};return oracle}
func TestOriginalEmbeddingResponseContract(t *testing.T){oracle:=readEmbeddingOracle(t);for _,test:=range oracle.Validators{t.Run(test.Name,func(t *testing.T){actual,err:=ValidateResponse(test.Protocol,test.Response,test.BatchLength);if test.Error!=""{if !errors.Is(err,ErrResponseInvalid)||actual!=nil{t.Fatal(actual,err)};return};if err!=nil||!reflect.DeepEqual(actual,test.Expected){t.Fatal(actual,test.Expected,err)}})}}
func TestOriginalEmbeddingRequestContract(t *testing.T){oracle:=readEmbeddingOracle(t);for _,test:=range oracle.Requests{t.Run(test.Name,func(t *testing.T){model:=Model{BaseURL:test.Model.BaseURL,APIKey:test.Model.APIKey,ModelName:test.Model.ModelName};actual,err:=ResolveURL(test.Protocol,model);if test.Error!=""{if err==nil||err.Error()!=test.Error{t.Fatal(actual,err)};return};if err!=nil||actual!=test.Request.URL{t.Fatal(actual,test.Request.URL,err)};_,modelName,err:=endpoint(test.Protocol,model);if err!=nil{t.Fatal(err)};raw,err:=requestBody(test.Protocol,modelName,test.Texts);if err!=nil{t.Fatal(err)};var body any;decoder:=json.NewDecoder(bytes.NewReader(raw));decoder.UseNumber();if err:=decoder.Decode(&body);err!=nil{t.Fatal(err)};if !reflect.DeepEqual(body,test.Request.Body){t.Fatal("request body differs",body,test.Request.Body)}})}}
func TestOriginalEmbeddingChunkContract(t *testing.T){oracle:=readEmbeddingOracle(t);rejected:=0;for _,test:=range oracle.Chunks{t.Run(test.Name,func(t *testing.T){actual,err:=ChunkText(context.Background(),test.Text,test.MaxChars,test.Overlap);if test.Error!=""{rejected++;if !test.OriginalContainsLoneSurrogate||!errors.Is(err,ErrChunkUTF16Boundary)||actual!=nil{t.Fatal("unpaired UTF16 boundary not rejected",actual,err)};return};if err!=nil||!reflect.DeepEqual(actual,test.Expected){t.Fatal(actual,test.Expected,err)}})};if rejected==0{t.Fatal("oracle missed deliberate UTF16 reject boundary")}}
// Request and batching oracles deliberately use original code with a fake endpoint.
// Real native HTTP and exact-origin safety are independently exercised by provider tests.
func TestOriginalEmbeddingBatchContractNativeHTTP(t *testing.T){oracle:=readEmbeddingOracle(t);for _,test:=range oracle.Batches{t.Run(test.Name,func(t *testing.T){lengths:=[]int{};var mutex sync.Mutex;server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){defer r.Body.Close();var body map[string]any;if err:=json.NewDecoder(r.Body).Decode(&body);err!=nil{t.Error(err);w.WriteHeader(400);return};texts:=[]string{};if test.Protocol==OpenAI{for _,text:=range body["input"].([]any){texts=append(texts,text.(string))}}else{for _,item:=range body["requests"].([]any){value:=item.(map[string]any)["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"].(string);texts=append(texts,value)}};mutex.Lock();lengths=append(lengths,len(texts));mutex.Unlock();values:=[]any{};for index,text:=range texts{value,err:=strconv.Atoi(text);if err!=nil{t.Error(err)};if test.Protocol==OpenAI{values=append(values,map[string]any{"index":index,"embedding":[]int{value,1}})}else{values=append(values,map[string]any{"values":[]int{value,1}})}};w.Header().Set("Content-Type","application/json");if test.Protocol==OpenAI{_ =json.NewEncoder(w).Encode(map[string]any{"data":values})}else{_ =json.NewEncoder(w).Encode(map[string]any{"embeddings":values})}}));defer server.Close();egressClient,err:=egress.New([]string{server.URL});if err!=nil{t.Fatal(err)};defer egressClient.Close();client:=New(egressClient);texts:=make([]string,test.TextCount);for i:=range texts{texts[i]=strconv.Itoa(i)};checks:=0;actual,err:=client.Generate(context.Background(),test.Protocol,Model{BaseURL:server.URL,APIKey:"synthetic-only"},texts,test.Size,func()error{checks++;return nil});mutex.Lock();actualLengths:=append([]int{},lengths...);mutex.Unlock();if err!=nil||!reflect.DeepEqual(actual,test.Expected)||!reflect.DeepEqual(actualLengths,test.BatchLengths)||checks!=test.AuthorityChecks{t.Fatal("native batching differed from actual original",actualLengths,test.BatchLengths,checks,test.AuthorityChecks,err)}})}}
