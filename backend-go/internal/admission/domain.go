package admission

import (
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "errors"
 "math"
 "sort"
 "strconv"
 "unicode/utf16"
 "unicode/utf8"
)

// DomainEvidence matches the Node migration verifier's domain(db): only nonempty
// tables are counted; rows use SQLite storage classes and safeIntegers wrappers,
// sort by JavaScript UTF-16 code units, then hash JSON([table,encodedRows]).
// This is distinct from physical DB/WAL hashes and schema admission authority.
type DomainEvidence struct {
 TableCounts map[string]int64 `json:"tableCounts"`
 AuthorContentHash string `json:"authorContentHash"`
}

const maxDomainEncodedBytes=64*1024*1024
var errDomainEncoding=errors.New("SCHEMA_DOMAIN_ENCODING_UNSUPPORTED")

func appendSQLiteValue(dst []byte,value any)([]byte,error){
 switch value:=value.(type){
 case nil:return append(dst,"null"...),nil
 case int64:
  dst=append(dst,`{"integer":"`...);dst=strconv.AppendInt(dst,value,10);return append(dst,`"}`...),nil
 case float64:
  if math.IsInf(value,0)||math.IsNaN(value){return append(dst,"null"...),nil}
  if value==0{return append(dst,'0'),nil}
  encoded,err:=json.Marshal(value);return append(dst,encoded...),err
 case bool:if value{return append(dst,"true"...),nil};return append(dst,"false"...),nil
 case string:
  if !utf8.ValidString(value){return nil,errDomainEncoding};return appendJSString(dst,value),nil
 case []byte:
  // better-sqlite3 returns a Buffer, whose JSON serialization preserves bytes.
  dst=append(dst,`{"type":"Buffer","data":[`...)
  for i,b:=range value{if i>0{dst=append(dst,',')};dst=strconv.AppendUint(dst,uint64(b),10)}
  return append(dst,']','}'),nil
 default:return nil,errDomainEncoding
 }
}
func lessUTF16(left,right string)bool{
 a,b:=utf16.Encode([]rune(left)),utf16.Encode([]rune(right));for i:=0;i<len(a)&&i<len(b);i++{if a[i]!=b[i]{return a[i]<b[i]}};return len(a)<len(b)
}

func logicalDomain(ctx context.Context,tx *sql.Tx,tables []string)(DomainEvidence,error){
 result:=DomainEvidence{TableCounts:make(map[string]int64)};digest:=sha256.New();names:=append([]string(nil),tables...);sort.Strings(names)
 var total int64
 for _,table:=range names{
  rows,err:=tx.QueryContext(ctx,"SELECT * FROM "+quoteIdentifier(table));if err!=nil{return result,err}
  columns,err:=rows.Columns();if err!=nil{rows.Close();return result,err}
  encodedRows:=[]string{}
  for rows.Next(){
   values:=make([]any,len(columns));pointers:=make([]any,len(columns));for i:=range values{pointers[i]=&values[i]}
   if err:=rows.Scan(pointers...);err!=nil{rows.Close();return result,err}
   encoded:=[]byte{'['}
   for i,value:=range values{if i>0{encoded=append(encoded,',')};encoded,err=appendSQLiteValue(encoded,value);if err!=nil{rows.Close();return result,err}}
   encoded=append(encoded,']');total+=int64(len(encoded));if total>maxDomainEncodedBytes{rows.Close();return result,errors.New("SCHEMA_DOMAIN_PROOF_TOO_LARGE")}
   encodedRows=append(encodedRows,string(encoded))
  }
  iterationErr:=rows.Err();closeErr:=rows.Close();if err:=errors.Join(iterationErr,closeErr);err!=nil{return result,err}
  if len(encodedRows)==0{continue}
  sort.Slice(encodedRows,func(i,j int)bool{return lessUTF16(encodedRows[i],encodedRows[j])})
  result.TableCounts[table]=int64(len(encodedRows))
  // Stream the outer JSON directly into the hash; avoid a second full-table copy.
  _,_=digest.Write(appendJSString([]byte{'['},table));_,_=digest.Write([]byte(",["))
  for i,row:=range encodedRows{if i>0{_,_=digest.Write([]byte{','})};_,_=digest.Write(appendJSString(nil,row))}
  _,_=digest.Write([]byte("]]"))
 }
 result.AuthorContentHash=hex.EncodeToString(digest.Sum(nil));return result,nil
}
