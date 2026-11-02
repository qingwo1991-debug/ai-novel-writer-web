// Package archiveformat is the portable format codec, not an export/import service.
// It has no filesystem paths, project session, publication or author-write authority.
package archiveformat

import (
 "bytes"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "math"
 "regexp"
 "strconv"
 "strings"
 "time"
 "unicode/utf16"
 "unicode/utf8"

 "golang.org/x/text/cases"
 "golang.org/x/text/language"
 "golang.org/x/text/unicode/norm"
)

const MaxManifestBytes = 16 * 1024 * 1024
const maxSafeInteger uint64 = 9007199254740991

type Error struct { Code string }
func (e *Error) Error() string { return e.Code }
func failure(code string) error { return &Error{Code: code} }
func Code(err error) string { var e *Error; if errors.As(err, &e) { return e.Code }; return "PORTABLE_ARCHIVE_INVALID" }

// Zero fields select defaults. Explicit invalid/zero JS overrides are checked by
// ParseLimits; Go callers use the strongly typed Limits value.
type Limits struct {
 MaxEntries uint64 `json:"maxEntries"`
 MaxEntryBytes uint64 `json:"maxEntryBytes"`
 MaxTotalBytes uint64 `json:"maxTotalBytes"`
 MaxDeclaredCompressedBytes uint64 `json:"maxDeclaredCompressedBytes"`
 MaxCompressionRatio uint64 `json:"maxCompressionRatio"`
 MaxPathBytes uint64 `json:"maxPathBytes"`
 MaxSemanticCounts uint64 `json:"maxSemanticCounts"`
 MaxSemanticCountValue uint64 `json:"maxSemanticCountValue"`
 MaxOmittedItems uint64 `json:"maxOmittedItems"`
 MaxReferenceIDs uint64 `json:"maxReferenceIds"`
}
func DefaultLimits() Limits { return Limits{100000, 2<<30, 16<<30, 16<<30, 200, 1024, 256, 1000000000, 100000, 100000} }
func (l Limits) resolved() (Limits,error) {
 d:=DefaultLimits(); fields:=[]*uint64{&l.MaxEntries,&l.MaxEntryBytes,&l.MaxTotalBytes,&l.MaxDeclaredCompressedBytes,&l.MaxCompressionRatio,&l.MaxPathBytes,&l.MaxSemanticCounts,&l.MaxSemanticCountValue,&l.MaxOmittedItems,&l.MaxReferenceIDs}
 defaults:=[]uint64{d.MaxEntries,d.MaxEntryBytes,d.MaxTotalBytes,d.MaxDeclaredCompressedBytes,d.MaxCompressionRatio,d.MaxPathBytes,d.MaxSemanticCounts,d.MaxSemanticCountValue,d.MaxOmittedItems,d.MaxReferenceIDs}
 for i,p:=range fields { if *p==0 { *p=defaults[i] }; if *p>maxSafeInteger { return l,failure("PORTABLE_ARCHIVE_LIMIT_INVALID") } }; return l,nil
}
func ParseLimits(data []byte) (Limits,error) {
 l:=DefaultLimits(); if len(data)==0 || string(data)=="null" { return l,nil }; v,e:=decodeJSON(data); if e!=nil { return l,failure("PORTABLE_ARCHIVE_LIMIT_INVALID") }; m,ok:=v.(map[string]any); if !ok { return l,failure("PORTABLE_ARCHIVE_LIMIT_INVALID") }
 fields:=map[string]*uint64{"maxEntries":&l.MaxEntries,"maxEntryBytes":&l.MaxEntryBytes,"maxTotalBytes":&l.MaxTotalBytes,"maxDeclaredCompressedBytes":&l.MaxDeclaredCompressedBytes,"maxCompressionRatio":&l.MaxCompressionRatio,"maxPathBytes":&l.MaxPathBytes,"maxSemanticCounts":&l.MaxSemanticCounts,"maxSemanticCountValue":&l.MaxSemanticCountValue,"maxOmittedItems":&l.MaxOmittedItems,"maxReferenceIds":&l.MaxReferenceIDs}
 for k,v:=range m { p,ok:=fields[k]; n,e:=integer(v,maxSafeInteger); if !ok || e!=nil || n==0 { return l,failure("PORTABLE_ARCHIVE_LIMIT_INVALID") }; *p=n }; return l,nil
}

type Entry struct { Path string `json:"path"`; ByteSize uint64 `json:"byteSize"`; SHA256 string `json:"sha256"`; Disposition string `json:"disposition"` }
type OmittedItem struct { ID string `json:"id"`; Reason string `json:"reason"` }
type Manifest struct {
 FormatVersion int `json:"formatVersion"`
 SourceSchemaVersion int `json:"sourceSchemaVersion"`
 OriginProjectID string `json:"originProjectId"`
 SnapshotGeneration string `json:"snapshotGeneration"`
 CreatedAt string `json:"createdAt"`
 DeclaredUncompressedBytes uint64 `json:"declaredUncompressedBytes"`
 DeclaredCompressedBytes uint64 `json:"declaredCompressedBytes"`
 Entries []Entry `json:"entries"`
 SemanticCounts map[string]uint64 `json:"semanticCounts"`
 OmittedItems []OmittedItem `json:"omittedItems"`
 TransferReceiptIDs []string `json:"transferReceiptIds"`
 HistoryProjectionIDs []string `json:"historyProjectionIds"`
}
var hashPattern=regexp.MustCompile(`^[a-f0-9]{64}$`)
var countPattern=regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
var reservedPattern=regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?$`)
var datePattern=regexp.MustCompile(`^(\d{4}|[+-]\d{6})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})\.(\d{3})Z$`)
func oneOf(s string, allowed ...string) bool { for _,a:=range allowed { if s==a { return true } }; return false }
func validDate(s string) bool {
 p:=datePattern.FindStringSubmatch(s); if p==nil { return false }; n:=make([]int,7); for i:=range n { n[i],_=strconv.Atoi(p[i+1]) }
 if p[1]=="-000000" || (len(p[1])==7 && n[0]>=0 && n[0]<=9999) { return false }
 t:=time.Date(n[0],time.Month(n[1]),n[2],n[3],n[4],n[5],n[6]*1000000,time.UTC)
 return t.Year()==n[0] && int(t.Month())==n[1] && t.Day()==n[2] && t.Hour()==n[3] && t.Minute()==n[4] && t.Second()==n[5] && t.UnixMilli()>=-8640000000000000 && t.UnixMilli()<=8640000000000000
}
func PathKey(value string, maxBytes int) (string,error) {
 if value=="" || !utf8.ValidString(value) || len(value)>maxBytes { return "",failure("PORTABLE_PATH_UNSAFE") }
 // The JS policy rejects *any* UTF-16 surrogate code unit, including paired
 // supplementary characters. Do not silently broaden this to all Unicode.
 for _,r:=range value { if r>0xffff { return "",failure("PORTABLE_PATH_UNSAFE") } }
 s:=norm.NFKC.String(value)
 if strings.HasPrefix(s,"/") || strings.Contains(s,"\\") { return "",failure("PORTABLE_PATH_UNSAFE") }
 for _,part:=range strings.Split(s,"/") {
  if part=="" || part=="." || part==".." || strings.HasSuffix(part,".") || strings.HasSuffix(part," ") || strings.ContainsAny(part,`<>:"|?*`) || reservedPattern.MatchString(part) { return "",failure("PORTABLE_PATH_UNSAFE") }
  for _,r:=range part { if r<=31 { return "",failure("PORTABLE_PATH_UNSAFE") } }
 }
 return cases.Lower(language.Und).String(s),nil
}
func integer(v any, max uint64) (uint64,error) {
 n,ok:=v.(json.Number); if !ok { return 0,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; f,e:=strconv.ParseFloat(string(n),64)
 if e!=nil || math.IsNaN(f) || math.IsInf(f,0) || f<0 || f>float64(maxSafeInteger) || f>float64(max) || math.Trunc(f)!=f { return 0,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; return uint64(f),nil
}
func identifier(v any) (string,error) { s,ok:=v.(string); if !ok || s=="" || len(utf16.Encode([]rune(s)))>512 || strings.ContainsRune(s,0) { return "",failure("PORTABLE_MANIFEST_INVALID") }; return s,nil }
func object(v any, keys ...string) (map[string]any,error) {
 m,ok:=v.(map[string]any); if !ok { return nil,failure("PORTABLE_MANIFEST_INVALID") }; if keys!=nil { if len(m)!=len(keys) { return nil,failure("PORTABLE_MANIFEST_INVALID") }; for _,k:=range keys { if _,ok:=m[k]; !ok { return nil,failure("PORTABLE_MANIFEST_INVALID") } } }; return m,nil
}
// Duplicate JSON object keys are rejected rather than hidden by last-key-wins
// decoding. The differential report records this deliberate native hardening.
func decodeJSON(data []byte) (any,error) {
 if !utf8.Valid(data) || !pairedJSONSurrogates(data) { return nil,failure("PORTABLE_MANIFEST_INVALID") }
 d:=json.NewDecoder(bytes.NewReader(data)); d.UseNumber()
 var read func(int)(any,error)
 read=func(depth int)(any,error) { if depth>128 { return nil,failure("PORTABLE_MANIFEST_INVALID") }; tok,e:=d.Token(); if e!=nil { return nil,e }; delim,ok:=tok.(json.Delim); if !ok { return tok,nil }
  switch delim {
  case '{': m:=map[string]any{}; for d.More() { k,e:=d.Token(); if e!=nil { return nil,e }; key,ok:=k.(string); if !ok { return nil,failure("PORTABLE_MANIFEST_INVALID") }; if _,ok:=m[key]; ok { return nil,failure("PORTABLE_MANIFEST_INVALID") }; v,e:=read(depth+1); if e!=nil { return nil,e }; m[key]=v }; end,e:=d.Token(); if e!=nil || end!=json.Delim('}') { return nil,failure("PORTABLE_MANIFEST_INVALID") }; return m,nil
  case '[': a:=[]any{}; for d.More() { v,e:=read(depth+1); if e!=nil { return nil,e }; a=append(a,v) }; end,e:=d.Token(); if e!=nil || end!=json.Delim(']') { return nil,failure("PORTABLE_MANIFEST_INVALID") }; return a,nil
  }; return nil,failure("PORTABLE_MANIFEST_INVALID")
 }
 v,e:=read(0); if e!=nil { return nil,e }; if _,e=d.Token(); e!=io.EOF { return nil,failure("PORTABLE_MANIFEST_INVALID") }; return v,nil
}
// encoding/json replaces lone escaped UTF-16 surrogates with U+FFFD. Reject
// those instead of accidentally admitting a path JS explicitly rejects. Paired
// surrogates decode normally; PathKey then applies the JS BMP-only policy.
func pairedJSONSurrogates(data []byte) bool {
 inString:=false
 for i:=0;i<len(data);i++ {
  if data[i]=='"' { inString=!inString; continue }; if !inString || data[i]!='\\' { continue }; i++; if i>=len(data) { return false }; if data[i]!='u' { continue }; if i+4>=len(data) { return false }
  n,e:=strconv.ParseUint(string(data[i+1:i+5]),16,16); if e!=nil { return false }; i+=4
  if n>=0xdc00&&n<=0xdfff { return false }; if n<0xd800||n>0xdbff { continue }
  if i+6>=len(data)||data[i+1]!='\\'||data[i+2]!='u' { return false }; low,e:=strconv.ParseUint(string(data[i+3:i+7]),16,16); if e!=nil||low<0xdc00||low>0xdfff { return false }; i+=6
 }; return true
}
func references(v any, max uint64) ([]string,error) {
 a,ok:=v.([]any); if !ok || uint64(len(a))>max { return nil,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; out:=make([]string,0,len(a)); seen:=map[string]bool{}
 for _,v:=range a { s,e:=identifier(v); if e!=nil { return nil,e }; if seen[s] { return nil,failure("PORTABLE_MANIFEST_INVALID") }; seen[s]=true; out=append(out,s) }; return out,nil
}
func ParseManifest(data []byte, configured Limits) (Manifest,error) {
 var result Manifest; l,e:=configured.resolved(); if e!=nil { return result,e }; if len(data)>MaxManifestBytes { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }
 value,e:=decodeJSON(data); if e!=nil { return result,failure("PORTABLE_MANIFEST_INVALID") }; m,e:=object(value,"formatVersion","sourceSchemaVersion","originProjectId","snapshotGeneration","createdAt","declaredUncompressedBytes","declaredCompressedBytes","entries","semanticCounts","omittedItems","transferReceiptIds","historyProjectionIds"); if e!=nil { return result,e }
 version,ve:=integer(m["formatVersion"],maxSafeInteger); schema,se:=integer(m["sourceSchemaVersion"],maxSafeInteger); if ve!=nil || se!=nil || version!=1 || schema!=7 { return result,failure("PORTABLE_SCHEMA_UNSUPPORTED") }; result.FormatVersion=1; result.SourceSchemaVersion=7
 if result.OriginProjectID,e=identifier(m["originProjectId"]); e!=nil { return result,e }; if result.SnapshotGeneration,e=identifier(m["snapshotGeneration"]); e!=nil { return result,e }
 result.CreatedAt,_=m["createdAt"].(string); if !validDate(result.CreatedAt) { return result,failure("PORTABLE_MANIFEST_INVALID") }
 if result.DeclaredUncompressedBytes,e=integer(m["declaredUncompressedBytes"],l.MaxTotalBytes); e!=nil { return result,e }; if result.DeclaredCompressedBytes,e=integer(m["declaredCompressedBytes"],l.MaxDeclaredCompressedBytes); e!=nil { return result,e }
 if ratioExceeded(result.DeclaredUncompressedBytes,result.DeclaredCompressedBytes,l.MaxCompressionRatio) { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }
 a,ok:=m["entries"].([]any); if !ok || uint64(len(a))>l.MaxEntries { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; result.Entries=make([]Entry,0,len(a)); paths:=map[string]bool{}; dirs:=map[string]bool{}; var total uint64
 for _,v:=range a {
  row,e:=object(v,"path","byteSize","sha256","disposition"); if e!=nil { return result,e }; p,ok:=row["path"].(string); if !ok { return result,failure("PORTABLE_PATH_UNSAFE") }; key,e:=PathKey(p,int(l.MaxPathBytes)); if e!=nil { return result,e }; if paths[key] || dirs[key] { return result,failure("PORTABLE_PATH_CONFLICT") }
  segments:=strings.Split(key,"/"); for i:=1;i<len(segments);i++ { d:=strings.Join(segments[:i],"/"); if paths[d] { return result,failure("PORTABLE_PATH_CONFLICT") }; dirs[d]=true }; paths[key]=true
  n,e:=integer(row["byteSize"],l.MaxEntryBytes); if e!=nil { return result,e }; h,_:=row["sha256"].(string); disposition,_:=row["disposition"].(string)
  if !hashPattern.MatchString(h) || !oneOf(disposition,"portable-database","author-content","knowledge-source","prompt","skill","avatar-asset","transfer-receipt","history-projection") { return result,failure("PORTABLE_MANIFEST_INVALID") }
  if n>l.MaxTotalBytes-total { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; total+=n; result.Entries=append(result.Entries,Entry{p,n,h,disposition})
 }
 if total!=result.DeclaredUncompressedBytes { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }
 counts,e:=object(m["semanticCounts"]); if e!=nil { return result,e }; if uint64(len(counts))>l.MaxSemanticCounts { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; result.SemanticCounts=map[string]uint64{}
 for k,v:=range counts { if !countPattern.MatchString(k) { return result,failure("PORTABLE_MANIFEST_INVALID") }; n,e:=integer(v,l.MaxSemanticCountValue); if e!=nil { return result,e }; result.SemanticCounts[k]=n }
 omitted,ok:=m["omittedItems"].([]any); if !ok || uint64(len(omitted))>l.MaxOmittedItems { return result,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; seen:=map[string]bool{}; result.OmittedItems=make([]OmittedItem,0,len(omitted))
 for _,v:=range omitted { row,e:=object(v,"id","reason"); if e!=nil { return result,e }; id,e:=identifier(row["id"]); if e!=nil { return result,e }; reason,_:=row["reason"].(string); if !oneOf(reason,"exclude-machine-authority","rebuild-stale","historical-nonreplayable","unsupported-safe-projection") || seen[id] { return result,failure("PORTABLE_MANIFEST_INVALID") }; seen[id]=true; result.OmittedItems=append(result.OmittedItems,OmittedItem{id,reason}) }
 if result.TransferReceiptIDs,e=references(m["transferReceiptIds"],l.MaxReferenceIDs); e!=nil { return result,e }; if result.HistoryProjectionIDs,e=references(m["historyProjectionIds"],l.MaxReferenceIDs); e!=nil { return result,e }; return result,nil
}
func ratioExceeded(uncompressed,compressed,ratio uint64) bool { if uncompressed>0 && compressed==0 { return true }; if compressed==0 { compressed=1 }; return float64(uncompressed)/float64(compressed)>float64(ratio) }
// marshalJS preserves struct key order, UTF-8 and JS JSON.stringify string
// escaping. Map key order is deterministic; no signature relies on map order.
func marshalJS(value any) ([]byte,error) { var b bytes.Buffer; enc:=json.NewEncoder(&b); enc.SetEscapeHTML(false); if e:=enc.Encode(value); e!=nil { return nil,e }; out:=bytes.TrimSuffix(b.Bytes(),[]byte{'\n'}); out=bytes.ReplaceAll(out,[]byte(`\u2028`),[]byte("\u2028")); out=bytes.ReplaceAll(out,[]byte(`\u2029`),[]byte("\u2029")); return out,nil }
func (m Manifest) Bytes(l Limits) ([]byte,error) { b,e:=marshalJS(m); if e!=nil { return nil,fmt.Errorf("manifest: %w",e) }; if _,e=ParseManifest(b,l); e!=nil { return nil,e }; return b,nil }
