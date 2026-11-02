package importsource

import (
 "bytes"
 "context"
 "crypto/hmac"
 "crypto/rand"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "sort"
 "strings"
 "unicode/utf8"

 "golang.org/x/text/unicode/norm"
)

type Purpose string
const (
 Reference Purpose = "reference"
 AuthorManuscript Purpose = "author-manuscript"
)
func validPurpose(p Purpose) bool { return p==Reference || p==AuthorManuscript }

// Identity is server-only input captured together with the uploaded/staged
// reader. Never decode this type from a browser request. No method opens it.
// FileIdentity identifies a trusted staging object, not a renderer-supplied hash.
type Identity struct {
 CanonicalLocation string `json:"-"`
 FileIdentity string `json:"-"`
}
type EncodedIdentity struct {
 LocationAliasDigest string `json:"-"`
 FileAliasDigest string `json:"-"`
}
type ResolvedIdentity struct {
 SourceIDs []string `json:"sourceIds"`
 SourceFingerprint string `json:"sourceFingerprint"`
 SourceFingerprints []string `json:"sourceFingerprints"`
}

func keyedDigest(key []byte,domain,value string) string {
 h:=hmac.New(sha256.New,key); _,_=h.Write([]byte(domain)); _,_=h.Write([]byte(value)); return hex.EncodeToString(h.Sum(nil))
}
func jsonExact(v any) string {
 var b bytes.Buffer; e:=json.NewEncoder(&b); e.SetEscapeHTML(false)
 if err:=e.Encode(v); err!=nil { panic("importsource: non-JSON internal value") }
 // JSON.stringify preserves these two characters, unlike encoding/json.
 return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSuffix(b.String(),"\n"),`\u2028`,"\u2028"),`\u2029`,"\u2029")
}
func uuid() (string,error) {
 var b [16]byte; if _,err:=rand.Read(b[:]); err!=nil { return "",err }; b[6]=(b[6]&0x0f)|0x40; b[8]=(b[8]&0x3f)|0x80
 return fmt.Sprintf("%x-%x-%x-%x-%x",b[0:4],b[4:6],b[6:8],b[8:10],b[10:]),nil
}
func validDigest(s string) bool {
 if len(s)!=64 { return false }; for _,c:=range s { if !(c>='0'&&c<='9'||c>='a'&&c<='f') { return false } }; return true
}

// EncodeIdentities ports the original source-alias domain and normalization.
// windows is chosen by the trusted host platform, never a renderer parameter.
// Purpose binds the later collection fingerprint, not the rename/replacement
// alias: a physical source retains its opaque ID across import purposes.
func EncodeIdentities(sources []Identity,purpose Purpose,key []byte,windows bool) ([]EncodedIdentity,error) {
 if !validPurpose(purpose) { return nil,errors.New("IMPORT_SOURCE_PURPOSE_INVALID") }
 if len(sources)==0 || len(sources)>MaxSourceFiles { return nil,errors.New("IMPORT_SOURCE_IDENTITY_INVALID") }
 if len(key)<32 { return nil,errors.New("IMPORT_SOURCE_SECRET_INVALID") }
 out:=make([]EncodedIdentity,0,len(sources))
 for _,source:=range sources {
  location:=norm.NFC.String(trim(source.CanonicalLocation)); file:=trim(source.FileIdentity)
  if !utf8.ValidString(location) || location=="" || utf16Length(location)>32000 || strings.ContainsRune(location,0) || !utf8.ValidString(file) || utf16Length(file)>1024 || strings.ContainsRune(file,0) || source.FileIdentity!="" && file=="" { return nil,errors.New("IMPORT_SOURCE_IDENTITY_INVALID") }
  if windows { location=strings.ToLower(location) }
  encoded:=EncodedIdentity{LocationAliasDigest:keyedDigest(key,"import-source-alias:v1:location:",location)}
  if file!="" { encoded.FileAliasDigest=keyedDigest(key,"import-source-alias:v1:file:",file) }; out=append(out,encoded)
 }
 return out,nil
}
func sourceFingerprint(ids []string,purpose Purpose,key []byte) string {
 sorted:=append([]string(nil),ids...); sort.Strings(sorted)
 value:=struct{ Version int `json:"version"`; Purpose Purpose `json:"purpose"`; SourceIDs []string `json:"sourceIds"` }{1,purpose,sorted}
 return keyedDigest(key,"",jsonExact(value))
}

// ResolveEncodedSources joins the ACTUAL existing import_source_aliases table
// inside a caller-owned transaction. It does not migrate, publish or commit.
// Raw identities/paths and the application key are never stored. New native
// inspection deliberately does not perform legacy encrypted-salt bridging;
// callers requiring that migration must keep their operation unpromoted.
func ResolveEncodedSources(ctx context.Context,tx *sql.Tx,sources []EncodedIdentity,purpose Purpose,key []byte) (ResolvedIdentity,error) {
 out:=ResolvedIdentity{SourceIDs:make([]string,0,len(sources)),SourceFingerprints:make([]string,0,len(sources))}
 if !validPurpose(purpose) { return out,errors.New("IMPORT_SOURCE_PURPOSE_INVALID") }
 if len(key)<32 { return out,errors.New("IMPORT_SOURCE_SECRET_INVALID") }
 if tx==nil { return out,errors.New("PROJECT_NOT_OPEN") }
 if len(sources)==0 || len(sources)>MaxSourceFiles { return out,errors.New("IMPORT_SOURCE_IDENTITY_INVALID") }
 for _,source:=range sources { if !validDigest(source.LocationAliasDigest) || source.FileAliasDigest!="" && !validDigest(source.FileAliasDigest) { return out,errors.New("IMPORT_SOURCE_IDENTITY_INVALID") } }
 find:=func(alias string) (string,error) {
  if alias=="" { return "",nil }; var id string
  err:=tx.QueryRowContext(ctx,"SELECT source_id FROM import_source_aliases WHERE alias_digest = ?",alias).Scan(&id)
  if errors.Is(err,sql.ErrNoRows) { return "",nil }; return id,err
 }
 upsert:=func(alias,kind,id string) error {
  if alias=="" { return nil }
  _,err:=tx.ExecContext(ctx,`INSERT INTO import_source_aliases (alias_digest, alias_kind, source_id) VALUES (?, ?, ?) ON CONFLICT(alias_digest) DO UPDATE SET alias_kind = excluded.alias_kind, source_id = excluded.source_id`,alias,kind,id)
  return err
 }
 for _,source:=range sources {
  locationID,err:=find(source.LocationAliasDigest); if err!=nil { return out,err }; fileID,err:=find(source.FileAliasDigest); if err!=nil { return out,err }
  id:=locationID; if id=="" { id=fileID }; if id=="" { id,err=uuid(); if err!=nil { return out,err } }
  if err=upsert(source.LocationAliasDigest,"location",id); err!=nil { return out,err }; if err=upsert(source.FileAliasDigest,"file",id); err!=nil { return out,err }
  out.SourceIDs=append(out.SourceIDs,id); out.SourceFingerprints=append(out.SourceFingerprints,sourceFingerprint([]string{id},purpose,key))
 }
 out.SourceFingerprint=sourceFingerprint(out.SourceIDs,purpose,key); return out,nil
}
