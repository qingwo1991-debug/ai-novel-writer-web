package importsource

import (
 "crypto/hmac"
 "errors"
 "io"
 "strings"
 "sync"
 "time"
)

// Binding MUST come from the authenticated server session/project lease, not
// browser-selected caller/generation strings. A generation switch invalidates
// old receipts even when a caller happens to retain the same browser session.
type Binding struct {
 Caller string `json:"caller"`
 Generation string `json:"generation"`
 Purpose Purpose `json:"purpose"`
}
// CapturedSource is accepted only from an upload/staging adapter that already
// owns this reader and identity. This package has no arbitrary path API.
type CapturedSource struct {
 DisplayName string
 Reader io.Reader
 Identity Identity
}
type Source struct {
 EncodedIdentity
 DisplayName string `json:"displayName"`
 MediaType string `json:"mediaType"`
 Size int64 `json:"size"`
}
type PreviewChapter struct {
 Number int `json:"number"`
 Title string `json:"title"`
 WordCount int `json:"wordCount"`
}
type Summary struct {
 InspectionID string `json:"inspectionId"`
 Purpose Purpose `json:"purpose"`
 SourceCount int `json:"sourceCount"`
 SourceDisplayNames []string `json:"sourceDisplayNames"`
 ChapterCount int `json:"chapterCount"`
 TotalWords int `json:"totalWords"`
 TotalBytes int64 `json:"totalBytes"`
 Preview []PreviewChapter `json:"preview"`
 // Compare-and-swap value commits the exact frozen preview+source/chapter
 // snapshot; it is opaque and keyed. The renderer never supplies a MAC key.
 PreviewCAS string `json:"previewCAS"`
 ExpiresAt int64 `json:"expiresAt"`
}
// Inspection is returned solely to a trusted internal consumer after one-use
// receipt validation. Sources' aliases are excluded from JSON at all levels.
type Inspection struct {
 Summary Summary
 Sources []Source
 Chapters []Chapter
}
type StoreOptions struct {
 Now func() time.Time
 TTL time.Duration
 MaxActive int
 MaxAggregateBytes int64
 Limits Limits
 WindowsIdentities bool
}
type storedInspection struct { binding Binding; id string; expires int64; retainedBytes int64; inspection Inspection }
type Store struct {
 mu sync.Mutex
 inspectMu sync.Mutex
 revisions map[string]uint64
 key []byte
 now func() time.Time
 ttl time.Duration
 maxActive int
 maxAggregateBytes int64
 limits Limits
 windows bool
 entries map[string]storedInspection
}
func NewStore(serverKey []byte,options StoreOptions) (*Store,error) {
 if len(serverKey)<32 { return nil,errors.New("IMPORT_SOURCE_SECRET_INVALID") }
 if options.Now==nil { options.Now=time.Now }; if options.TTL==0 { options.TTL=10*time.Minute }; if options.MaxActive==0 { options.MaxActive=2 }; if options.MaxAggregateBytes==0 { options.MaxAggregateBytes=MaxTotalBytes }; if options.Limits==(Limits{}) { options.Limits=DefaultLimits() }
 if options.TTL<=0 || options.TTL>10*time.Minute || options.MaxActive<1 || options.MaxActive>100 || options.MaxAggregateBytes<1 || options.MaxAggregateBytes>MaxTotalBytes || !validLimits(options.Limits) { return nil,errors.New("IMPORT_RESOURCE_LIMIT_INVALID") }
 return &Store{key:append([]byte(nil),serverKey...),now:options.Now,ttl:options.TTL,maxActive:options.MaxActive,maxAggregateBytes:options.MaxAggregateBytes,limits:options.Limits,windows:options.WindowsIdentities,entries:map[string]storedInspection{},revisions:map[string]uint64{}},nil
}
func validBinding(b Binding) bool { return trim(b.Caller)!="" && trim(b.Generation)!="" && len(b.Caller)<=1024 && len(b.Generation)<=1024 && !strings.ContainsRune(b.Caller,0) && !strings.ContainsRune(b.Generation,0) && validPurpose(b.Purpose) }
func (s *Store) removeExpiredLocked() { now:=s.now().UnixMilli(); for id,entry:=range s.entries { if entry.expires<=now { delete(s.entries,id) } } }
func (s *Store) receipt(entry storedInspection) string {
 value:=struct { ID string `json:"id"`; Binding Binding `json:"binding"`; Expiry int64 `json:"expiry"` }{entry.id,entry.binding,entry.expires}
 return entry.id+"."+keyedDigest(s.key,"import-inspection-receipt:v1:",jsonExact(value))
}
func cloneSummary(summary Summary) Summary { summary.SourceDisplayNames=append([]string{},summary.SourceDisplayNames...); summary.Preview=append([]PreviewChapter{},summary.Preview...); return summary }
func cloneInspection(i Inspection) Inspection { i.Summary=cloneSummary(i.Summary); i.Sources=append([]Source{},i.Sources...); i.Chapters=append([]Chapter{},i.Chapters...); return i }

// Inspect freezes reader bytes now. Caller order is the captured server order
// (the upload adapter must order multipart sources before calling if desired).
// A failed reselection revokes the old caller receipt, as the TS controller does.
func (s *Store) Inspect(binding Binding,captured []CapturedSource) (Summary,error) {
 if !validBinding(binding) { return Summary{},errors.New("IMPORT_INSPECTION_INVALID") }
 // Bound concurrent transient parsing memory as well as retained snapshots.
 s.inspectMu.Lock(); defer s.inspectMu.Unlock()
 s.RevokeCaller(binding.Caller)
 s.mu.Lock(); revision:=uint64(1); s.revisions[binding.Caller]=revision; s.mu.Unlock()
 defer func(){ s.mu.Lock(); delete(s.revisions,binding.Caller); s.mu.Unlock() }()
 if len(captured)==0 { return Summary{},errors.New("IMPORT_INSPECTION_INVALID") }; if len(captured)>s.limits.MaxSourceFiles { return Summary{},errors.New("IMPORT_SOURCE_COUNT_EXCEEDED") }
 identities:=make([]Identity,len(captured)); for i,c:=range captured { identities[i]=c.Identity }
 encoded,err:=EncodeIdentities(identities,binding.Purpose,s.key,s.windows); if err!=nil { return Summary{},err }
 inspection:=Inspection{Sources:make([]Source,0,len(captured)),Chapters:make([]Chapter,0)}; consumed:=int64(0); rawConsumed:=int64(0); words:=0; contentBytes:=int64(0); globalNumbers:=map[int]bool{}
 for index,source:=range captured {
  if consumed>=s.limits.MaxTotalBytes || rawConsumed>=s.limits.MaxTotalBytes { return Summary{},errors.New("IMPORT_SOURCE_BYTES_EXCEEDED") }
  remaining:=s.limits; remaining.MaxTotalBytes-=consumed
  if rawRemaining:=s.limits.MaxTotalBytes-rawConsumed; rawRemaining<remaining.MaxTotalBytes { remaining.MaxTotalBytes=rawRemaining }
  if len(inspection.Chapters)>=remaining.MaxChapters { return Summary{},errors.New("IMPORT_CHAPTER_COUNT_EXCEEDED") }; remaining.MaxChapters-=len(inspection.Chapters)
  parsed,e:=Parse(source.DisplayName,source.Reader,remaining); if e!=nil { return Summary{},e }; consumed+=parsed.Size; rawConsumed+=parsed.RawSize
  inspection.Sources=append(inspection.Sources,Source{EncodedIdentity:encoded[index],DisplayName:parsed.DisplayName,MediaType:parsed.MediaType,Size:parsed.Size})
  used:=map[int]bool{}; next:=1
  for _,chapter:=range parsed.Chapters {
   local:=chapter.Number; if local<1 || used[local] { for used[next] { next++ }; local=next }; used[local]=true
   chapter.SourceIndex=index; chapter.SourceChapterNumber=local
   if binding.Purpose==Reference { chapter.Number=len(inspection.Chapters)+1 } else if chapter.Number<1 || globalNumbers[chapter.Number] { return Summary{},errors.New("AUTHOR_MANUSCRIPT_DUPLICATE_CHAPTER") }
   globalNumbers[chapter.Number]=true; words+=chapter.WordCount; contentBytes+=int64(chapter.ContentSize)
   if contentBytes>s.limits.MaxTotalBytes { return Summary{},errors.New("IMPORT_SOURCE_BYTES_EXCEEDED") }
   inspection.Chapters=append(inspection.Chapters,chapter)
  }
 }
 id,err:=uuid(); if err!=nil { return Summary{},err }
 summary:=Summary{Purpose:binding.Purpose,SourceCount:len(inspection.Sources),SourceDisplayNames:make([]string,0,len(inspection.Sources)),ChapterCount:len(inspection.Chapters),TotalWords:words,TotalBytes:contentBytes,Preview:make([]PreviewChapter,0,8)}
 for _,source:=range inspection.Sources { summary.SourceDisplayNames=append(summary.SourceDisplayNames,source.DisplayName) }
 for i,chapter:=range inspection.Chapters { if i>=8 { break }; summary.Preview=append(summary.Preview,PreviewChapter{chapter.Number,chapter.Title,chapter.WordCount}) }
 // Include all attacker-influenced retained strings, not just prose bodies:
 // enormous title-only headings cannot bypass the aggregate memory budget.
 retainedSize:=contentBytes
 for _,chapter:=range inspection.Chapters { retainedSize+=int64(len(chapter.Title)) }
 for _,source:=range inspection.Sources { retainedSize+=int64(len(source.DisplayName)+len(source.MediaType)+len(source.LocationAliasDigest)+len(source.FileAliasDigest)) }
 s.mu.Lock(); defer s.mu.Unlock(); s.removeExpiredLocked()
 if s.revisions[binding.Caller]!=revision { return Summary{},errors.New("IMPORT_INSPECTION_EXPIRED") }
 retainedBytes:=int64(0); retainedCount:=0
 for _,entry:=range s.entries { if entry.binding.Caller!=binding.Caller { retainedCount++; retainedBytes+=entry.retainedBytes } }
 if retainedSize>s.maxAggregateBytes-retainedBytes { return Summary{},errors.New("IMPORT_SOURCE_BYTES_EXCEEDED") }; if retainedCount>=s.maxActive { return Summary{},errors.New("IMPORT_INSPECTION_LIMIT") }
 for key,entry:=range s.entries { if entry.binding.Caller==binding.Caller { delete(s.entries,key) } }
 entry:=storedInspection{binding:binding,id:id,expires:s.now().Add(s.ttl).UnixMilli(),retainedBytes:retainedSize}
 summary.ExpiresAt=entry.expires; summary.InspectionID=s.receipt(entry)
 // Bind CAS to all source aliases and complete chapter content, not merely the
 // truncated eight-chapter preview. Alias fields are explicitly serialized here
 // only into a server-side HMAC input, never returned to a renderer.
 aliases:=make([][2]string,len(inspection.Sources)); for i,source:=range inspection.Sources { aliases[i]=[2]string{source.LocationAliasDigest,source.FileAliasDigest} }
 casValue:=struct { Receipt string; Binding Binding; Summary Summary; Aliases [][2]string; Chapters []Chapter }{summary.InspectionID,binding,summary,aliases,inspection.Chapters}
 summary.PreviewCAS=keyedDigest(s.key,"import-inspection-preview:v1:",jsonExact(casValue))
 inspection.Summary=summary; entry.inspection=inspection; s.entries[id]=entry
 return cloneSummary(summary),nil
}
func (s *Store) lookupLocked(receipt string,binding Binding) (storedInspection,error) {
 s.removeExpiredLocked(); id,_,ok:=strings.Cut(receipt,"."); entry,exists:=s.entries[id]
 if !ok || !exists || !validBinding(binding) || entry.binding!=binding || !hmac.Equal([]byte(s.receipt(entry)),[]byte(receipt)) { return storedInspection{},errors.New("IMPORT_INSPECTION_EXPIRED") }; return entry,nil
}
// Peek returns only safe metadata, never canonical identity or chapter bodies.
func (s *Store) Peek(receipt string,binding Binding) (Summary,error) {
 s.mu.Lock(); defer s.mu.Unlock(); entry,err:=s.lookupLocked(receipt,binding); if err!=nil { return Summary{},err }; return cloneSummary(entry.inspection.Summary),nil
}
// Consume atomically validates purpose/caller/generation/expiry and preview CAS,
// deletes the receipt, and returns a frozen copy. A CAS failure does not consume.
// This is NOT an import commit: the consumer still needs public integration,
// transaction/lease checks, durable run state, effect receipts and publication.
func (s *Store) Consume(receipt,expectedPreviewCAS string,binding Binding) (Inspection,error) {
 s.mu.Lock(); defer s.mu.Unlock(); entry,err:=s.lookupLocked(receipt,binding); if err!=nil { return Inspection{},err }
 if !validDigest(expectedPreviewCAS) || !hmac.Equal([]byte(entry.inspection.Summary.PreviewCAS),[]byte(expectedPreviewCAS)) { return Inspection{},errors.New("IMPORT_INSPECTION_PREVIEW_CHANGED") }
 delete(s.entries,entry.id); return cloneInspection(entry.inspection),nil
}
func (s *Store) RevokeCaller(caller string) { s.mu.Lock(); defer s.mu.Unlock(); if _,active:=s.revisions[caller]; active { s.revisions[caller]++ }; for id,entry:=range s.entries { if entry.binding.Caller==caller { delete(s.entries,id) } } }
func (s *Store) ActiveCount() int { s.mu.Lock(); defer s.mu.Unlock(); s.removeExpiredLocked(); return len(s.entries) }
