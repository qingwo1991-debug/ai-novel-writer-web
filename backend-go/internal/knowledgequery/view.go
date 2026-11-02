// Package knowledgequery provides immutable, bounded read business projections
// from a native-admitted private knowledge snapshot. It neither connects to Lance
// nor grants source-authority, writes, legacy registry inference, search or AI use.
// Callers must bind snapshot and copied root to the same independently proven
// physical stage and exclude writers until construction and final proof complete.
package knowledgequery

import (
 "context"
 "bytes"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "os"
 "strconv"
 "unicode/utf8"

 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
 "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

var (
 ErrInput=errors.New("KNOWLEDGE_QUERY_INPUT_INVALID")
 ErrDomain=errors.New("KNOWLEDGE_QUERY_DOMAIN_INVALID")
 ErrCopies=errors.New("KNOWLEDGE_QUERY_COPIES_INVALID")
)

type document struct{id,fileName,importedAt,filePath,corpusKind string;chunkCount int64}
type capturedCopy struct{content,hash,status string;edited bool}
// View retains no caller maps, slices, raw JSON buffers, roots or file handles.
// All public reads return fresh containers, and concurrent reads perform no I/O.
type View struct{documents []document;copies map[string]capturedCopy;documentCount,totalChunks,vectorDimension,vectorless int}

func check(ctx context.Context)error{if ctx==nil{return ErrInput};return ctx.Err()}
func normalize(err,errorClass error)error{if errors.Is(err,context.Canceled){return context.Canceled};if errors.Is(err,context.DeadlineExceeded){return context.DeadlineExceeded};return errorClass}

// New validates the typed snapshot first at fixed experimental hard bounds,
// validates/captures all managed copied metadata, then projects immutable reads.
// Every failure returns nil and symbolic errors. This is not author admission;
// source/copy equality, quiescence, ownership and final proof are caller duties.
func New(ctx context.Context,snapshot knowledgedomain.Snapshot,root *os.Root,budget knowledgecopy.Budget)(*View,error){
 if err:=check(ctx);err!=nil{return nil,err}
 if _,err:=knowledgedomain.Validate(ctx,snapshot,knowledgedomain.Budget{MaxTables:32,MaxRows:10000,MaxCells:200000,MaxTextBytes:32*1024*1024});err!=nil{return nil,normalize(err,ErrDomain)}
 out:=&View{documents:[]document{},copies:map[string]capturedCopy{}}
 refs:=[]knowledgecopy.DocumentRef{}
 var chunks []map[string]any
 tables:=make(map[string][]map[string]any,len(snapshot.Tables))
 chunksPresent:=false
 for _,table:=range snapshot.Tables{if err:=ctx.Err();err!=nil{return nil,err};tables[table.Name]=table.Records;switch table.Name{case "chunks":chunksPresent=true;chunks=table.Records;case "documents":for _,row:=range table.Records{if err:=ctx.Err();err!=nil{return nil,err};kind,_:=row["corpusKind"].(string);if kind==""{kind="unknown"};doc:=document{id:row["id"].(string),fileName:row["fileName"].(string),importedAt:row["importedAt"].(string),filePath:row["filePath"].(string),corpusKind:kind,chunkCount:integer(row["chunkCount"])};out.documents=append(out.documents,doc);refs=append(refs,knowledgecopy.DocumentRef{ID:doc.id,FilePath:doc.filePath})}}}
 copies,err:=knowledgecopy.ReadManagedCopies(ctx,root,refs,budget);if err!=nil{return nil,normalize(err,ErrCopies)}
 for id,copy:=range copies{if err:=ctx.Err();err!=nil{return nil,err};sum:=sha256.Sum256([]byte(copy.Content));hash:=hex.EncodeToString(sum[:]);status:="stale";if !copy.IndexDirty&&hash==copy.IndexedHash{status="current"};out.copies[id]=capturedCopy{content:copy.Content,hash:hash,status:status,edited:copy.Edited}}
 if chunksPresent{
  out.documentCount=len(out.documents);out.totalChunks=len(chunks)
  var registry struct{ActiveGeneration *json.Number `json:"activeGeneration"`;Spaces []struct{Generation json.Number `json:"generation"`;TableName string `json:"tableName"`;Status string `json:"status"`;VectorDimension json.Number `json:"vectorDimension"`} `json:"spaces"`}
  if len(snapshot.Registry)>0{decoder:=json.NewDecoder(bytes.NewReader(snapshot.Registry));decoder.UseNumber();if err:=decoder.Decode(&registry);err!=nil{return nil,ErrDomain}}
  selected:="";for _,space:=range registry.Spaces{if registry.ActiveGeneration!=nil&&integer(space.Generation)==integer(*registry.ActiveGeneration)&&space.Status=="active"{selected=space.TableName;out.vectorDimension=int(integer(space.VectorDimension));break}}
  if selected==""{for _,space:=range registry.Spaces{if space.Status=="building"{selected=space.TableName;break}}}
  vectorised:=map[string]bool{};if selected!=""{for _,row:=range tables[selected]{if err:=ctx.Err();err!=nil{return nil,err};vectorised[row["id"].(string)]=true}}
  for _,row:=range chunks{if err:=ctx.Err();err!=nil{return nil,err};if !vectorised[row["id"].(string)]{out.vectorless++}}
 }
 if err:=ctx.Err();err!=nil{return nil,err};return out,nil
}

func integer(value any)int64{switch v:=value.(type){case json.Number:n,_:=strconv.ParseFloat(string(v),64);return int64(n);case int:return int64(v);case int8:return int64(v);case int16:return int64(v);case int32:return int64(v);case int64:return v;case uint:return int64(v);case uint8:return int64(v);case uint16:return int64(v);case uint32:return int64(v);case uint64:return int64(v);case float32:return int64(v);case float64:return int64(v)};return 0}

func (v *View)Documents(ctx context.Context)([]map[string]any,error){if err:=check(ctx);err!=nil{return nil,err};if v==nil{return nil,ErrInput};out:=make([]map[string]any,0,len(v.documents));for _,doc:=range v.documents{if err:=ctx.Err();err!=nil{return nil,err};out=append(out,map[string]any{"id":doc.id,"fileName":doc.fileName,"importedAt":doc.importedAt,"chunkCount":doc.chunkCount,"filePath":doc.filePath,"corpusKind":doc.corpusKind})};return out,nil}
// Stats is the public knowledge-base projection: hasVectors is deliberately not
// exposed (the internal vector-store result has a fourth, nonpublic field).
func (v *View)Stats(ctx context.Context)(map[string]any,error){if err:=check(ctx);err!=nil{return nil,err};if v==nil{return nil,ErrInput};return map[string]any{"documentCount":v.documentCount,"totalChunks":v.totalChunks,"vectorDimension":v.vectorDimension},nil}
func (v *View)VectorlessCount(ctx context.Context)(map[string]any,error){if err:=check(ctx);err!=nil{return nil,err};if v==nil{return nil,ErrInput};return map[string]any{"count":v.vectorless},nil}
func (v *View)ReadCopy(ctx context.Context,id string)(map[string]any,error){if err:=check(ctx);err!=nil{return nil,err};if v==nil||len(id)>32*1024*1024||!utf8.ValidString(id){return nil,ErrInput};copy,ok:=v.copies[id];if !ok{return map[string]any{"available":false,"indexStatus":"unavailable"},nil};return map[string]any{"available":true,"content":copy.content,"contentHash":copy.hash,"edited":copy.edited,"indexStatus":copy.status},nil}

