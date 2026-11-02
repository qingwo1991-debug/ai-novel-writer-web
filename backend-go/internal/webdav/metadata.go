// Package webdav implements the append-only v1 DAV generation protocol. It owns
// neither credential persistence, project binding nor archive publication/restore.
package webdav

import (
 "bytes"
 "encoding/json"
 "io"
 "regexp"
 "sort"
 "time"
 "unicode/utf8"
)

// Error is intentionally only a stable code. Remote bodies, URLs and transport
// errors (which may contain credentials) are never exposed or wrapped.
type Error string
func (e Error) Error() string { return string(e) }
const (
 InputInvalid Error = "WEBDAV_INPUT_INVALID"
 Auth Error = "WEBDAV_AUTH"
 Network Error = "WEBDAV_NETWORK"
 NotFound Error = "WEBDAV_NOT_FOUND"
 RemoteConflict Error = "WEBDAV_REMOTE_CONFLICT"
 RemoteInvalid Error = "WEBDAV_REMOTE_INVALID"
 DAVResponseInvalid Error = "WEBDAV_DAV_RESPONSE_INVALID"
 ResponseTooLarge Error = "WEBDAV_RESPONSE_TOO_LARGE"
 Cancelled Error = "WEBDAV_CANCELLED"
)
const maxSafeInteger int64 = 9007199254740991
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@+\-]{0,511}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Descriptor field order is the original JSON.stringify wire order.
type ArchiveDescriptor struct {
 File string `json:"file"`
 SHA256 string `json:"sha256"`
 ByteSize int64 `json:"byteSize"`
}
type ManifestDescriptor struct {
 File string `json:"file"`
 SHA256 string `json:"sha256"`
 ByteSize int64 `json:"byteSize"`
}
type Manifest struct {
 Version int `json:"version"`
 CloudBookID string `json:"cloudBookId"`
 GenerationID string `json:"generationId"`
 ParentGenerationIDs []string `json:"parentGenerationIds"`
 CreatedAt string `json:"createdAt"`
 OriginProjectID string `json:"originProjectId"`
 PortableSnapshotGeneration string `json:"portableSnapshotGeneration"`
 Archive ArchiveDescriptor `json:"archive"`
}
type Completion struct {
 Version int `json:"version"`
 CloudBookID string `json:"cloudBookId"`
 GenerationID string `json:"generationId"`
 Manifest ManifestDescriptor `json:"manifest"`
 Archive ArchiveDescriptor `json:"archive"`
 CompletedAt string `json:"completedAt"`
}
type Generation struct {
 CloudBookID string `json:"cloudBookId"`
 GenerationID string `json:"generationId"`
 ParentGenerationIDs []string `json:"parentGenerationIds"`
 CreatedAt string `json:"createdAt"`
 OriginProjectID string `json:"originProjectId"`
 PortableSnapshotGeneration string `json:"portableSnapshotGeneration"`
 ArchiveSHA256 string `json:"archiveSha256"`
 ArchiveByteSize int64 `json:"archiveByteSize"`
 HasSibling bool `json:"hasSibling"`
 SiblingGenerationIDs []string `json:"siblingGenerationIds"`
}
func validTime(s string) bool {
 if len(s) != 24 { return false }
 t, err := time.Parse("2006-01-02T15:04:05.000Z", s)
 return err == nil && t.Format("2006-01-02T15:04:05.000Z") == s
}
func validParents(p []string) bool {
 if p == nil || len(p) > 256 { return false }
 seen := make(map[string]bool, len(p))
 for _, id := range p { if !idPattern.MatchString(id) || seen[id] { return false }; seen[id] = true }
 return true
}
func validArchive(a ArchiveDescriptor) bool {
 return a.File == "archive.ainovel" && hashPattern.MatchString(a.SHA256) && a.ByteSize >= 0 && a.ByteSize <= maxSafeInteger
}
// Strict JSON keys reject missing, extra, duplicate, null, invalid UTF-8 and
// trailing values; recursive depth is capped before allocating typed metadata.
func strictJSON(b []byte, target any) error {
 if !utf8.Valid(b) { return RemoteInvalid }
 d := json.NewDecoder(bytes.NewReader(b)); d.UseNumber()
 var walk func(int) error
 walk = func(depth int) error {
  if depth > 32 { return RemoteInvalid }
  token, err := d.Token(); if err != nil || token == nil { return RemoteInvalid }
  delim, isDelim := token.(json.Delim); if !isDelim { return nil }
  switch delim {
  case '{':
   seen := map[string]bool{}
   for d.More() { k, err := d.Token(); if err != nil { return RemoteInvalid }; key, ok := k.(string); if !ok || seen[key] { return RemoteInvalid }; seen[key] = true; if err := walk(depth+1); err != nil { return err } }
  case '[':
   for d.More() { if err := walk(depth+1); err != nil { return err } }
  default: return RemoteInvalid
  }
  _, err = d.Token(); if err != nil { return RemoteInvalid }; return nil
 }
 if err := walk(0); err != nil { return err }
 if _, err := d.Token(); err != io.EOF { return RemoteInvalid }
 decoder := json.NewDecoder(bytes.NewReader(b)); decoder.DisallowUnknownFields()
 if err := decoder.Decode(target); err != nil { return RemoteInvalid }
 return nil
}
func keys(b []byte, expected ...string) bool {
 var m map[string]json.RawMessage
 if json.Unmarshal(b, &m) != nil || len(m) != len(expected) { return false }
 for _, k := range expected { if _, ok := m[k]; !ok { return false } }
 return true
}
func nested(b []byte, key string) []byte { var m map[string]json.RawMessage; _ = json.Unmarshal(b, &m); return m[key] }
func ParseManifest(b []byte) (Manifest, error) {
 var m Manifest
 if len(b)>16<<20 { return m,ResponseTooLarge }
 if !keys(b, "version", "cloudBookId", "generationId", "parentGenerationIds", "createdAt", "originProjectId", "portableSnapshotGeneration", "archive") || !keys(nested(b,"archive"),"file","sha256","byteSize") { return m, RemoteInvalid }
 if err := strictJSON(b, &m); err != nil { return Manifest{}, err }
 if m.Version != 1 || !idPattern.MatchString(m.CloudBookID) || !uuidPattern.MatchString(m.GenerationID) || !uuidPattern.MatchString(m.OriginProjectID) || !idPattern.MatchString(m.PortableSnapshotGeneration) || !validTime(m.CreatedAt) || !validParents(m.ParentGenerationIDs) || !validArchive(m.Archive) { return Manifest{}, RemoteInvalid }
 return m, nil
}
func ParseCompletion(b []byte) (Completion, error) {
 var c Completion
 if len(b)>16<<20 { return c,ResponseTooLarge }
 if !keys(b,"version","cloudBookId","generationId","manifest","archive","completedAt") || !keys(nested(b,"archive"),"file","sha256","byteSize") || !keys(nested(b,"manifest"),"file","sha256","byteSize") { return c, RemoteInvalid }
 if err := strictJSON(b,&c); err != nil { return Completion{}, err }
 if c.Version != 1 || !idPattern.MatchString(c.CloudBookID) || !uuidPattern.MatchString(c.GenerationID) || !validTime(c.CompletedAt) || !validArchive(c.Archive) || c.Manifest.File != "manifest.json" || !hashPattern.MatchString(c.Manifest.SHA256) || c.Manifest.ByteSize < 0 || c.Manifest.ByteSize > maxSafeInteger { return Completion{}, RemoteInvalid }
 return c,nil
}
func wireJSON(v any) ([]byte,error) {
 var b bytes.Buffer
 e := json.NewEncoder(&b); e.SetEscapeHTML(false)
 if err := e.Encode(v); err != nil { return nil,InputInvalid }
 return bytes.TrimSuffix(b.Bytes(),[]byte{'\n'}),nil
}
func publicGeneration(m Manifest, siblings []string) Generation {
 return Generation{m.CloudBookID,m.GenerationID,append([]string{},m.ParentGenerationIDs...),m.CreatedAt,m.OriginProjectID,m.PortableSnapshotGeneration,m.Archive.SHA256,m.Archive.ByteSize,len(siblings)>0,append([]string{},siblings...)}
}
// Catalog preserves DAV ordering and stored parent order. Siblings compare parent
// sets, not click order, and no mutable latest pointer is used.
func Catalog(manifests []Manifest) []Generation {
 key := func(m Manifest) string { p := append([]string{},m.ParentGenerationIDs...); sort.Strings(p); b,_ := wireJSON(p); return string(b) }
 groups := map[string][]string{}
 for _, m := range manifests { k := key(m); groups[k] = append(groups[k],m.GenerationID) }
 out := make([]Generation,0,len(manifests))
 for _, m := range manifests { siblings := []string{}; for _, id := range groups[key(m)] { if id != m.GenerationID { siblings = append(siblings,id) } }; out = append(out,publicGeneration(m,siblings)) }
 return out
}
