package importsource

import (
 "archive/zip"
 "bytes"
 "errors"
 "io"
 "net/url"
 "path"
 "regexp"
 "strconv"
 "strings"
 "unicode/utf16"
 "unicode/utf8"
)

const (
 EPUBMaxEntries = 10000
 EPUBMaxEntryBytes = 32 * 1024 * 1024
 EPUBMaxExtractedBytes = 128 * 1024 * 1024
)

type epubChapter struct { title,content string }
type epubManifest struct { href,mediaType,properties string }
func epubInvalid() error { return errors.New("EPUB_INVALID_ARCHIVE") }
func epubExpansion() error { return errors.New("EPUB_EXPANSION_LIMIT") }
func epubDRM() error { return errors.New("EPUB_DRM_UNSUPPORTED") }

// ZIP member names are never written to disk. Reject traversal, absolute,
// backslash, drive/URL and duplicate normalized names rather than repairing them.
func archivePath(s string) (string,error) {
 if s=="" || strings.ContainsAny(s,"\x00\\") || strings.HasPrefix(s,"/") || strings.Contains(s,":") { return "",epubInvalid() }
 clean:=path.Clean(s)
 if clean=="." || clean==".." || strings.HasPrefix(clean,"../") { return "",epubInvalid() }
 return clean,nil
}
func referencePath(dir,href string) (string,error) {
 if i:=strings.IndexAny(href,"?#"); i>=0 { href=href[:i] }
 decoded,err:=url.PathUnescape(href); if err!=nil || decoded=="" || strings.HasPrefix(decoded,"/") || strings.ContainsAny(decoded,"\x00\\:") { return "",epubInvalid() }
 return archivePath(path.Join(dir,decoded))
}

var (
 entityPattern = regexp.MustCompile(`(?i)&(#x[0-9a-f]+|#[0-9]+|[a-z]+);`)
 attributePattern = regexp.MustCompile(`([\w:-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
 commentPattern = regexp.MustCompile(`(?s)<!--[\s\S]*?-->`)
 scriptStylePattern = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>[\s\S]*?</(?:script|style)>`)
 brPattern = regexp.MustCompile(`(?i)<br\s*/?>`)
 blockPattern = regexp.MustCompile(`(?i)</?(?:address|article|aside|blockquote|div|h[1-6]|header|li|main|nav|ol|p|pre|section|table|tr|ul)\b[^>]*>`)
 tagPattern = regexp.MustCompile(`<[^>]+>`)
 horizontalPattern = regexp.MustCompile(`[\t ]+`)
 newlinesPattern = regexp.MustCompile(`\n{3,}`)
 bodyPattern = regexp.MustCompile(`(?is)<(?:[\w-]+:)?body\b[^>]*>([\s\S]*?)</(?:[\w-]+:)?body>`)
 titlePattern = regexp.MustCompile(`(?is)<(?:[\w-]+:)?title\b[^>]*>([\s\S]*?)</(?:[\w-]+:)?title>`)
 headingPattern = regexp.MustCompile(`(?is)<h[1-6]\b[^>]*>([\s\S]*?)</h[1-6]>`)
 encryptedBlockPattern = regexp.MustCompile(`(?is)<(?:[\w-]+:)?EncryptedData\b[^>]*>([\s\S]*?)</(?:[\w-]+:)?EncryptedData\s*>`)
 encryptedStartPattern = regexp.MustCompile(`(?i)<(?:[\w-]+:)?EncryptedData\b`)
)
func decodeEntities(s string) string {
 names:=map[string]string{"amp":"&","apos":"'","gt":">","lt":"<","nbsp":" ","quot":"\""}
 return entityPattern.ReplaceAllStringFunc(s,func(entity string) string {
  body:=entity[1:len(entity)-1]
  if !strings.HasPrefix(body,"#") { if v,ok:=names[strings.ToLower(body)]; ok { return v }; return entity }
  base,start:=10,1; if len(body)>1 && (body[1]=='x'||body[1]=='X') { base=16; start=2 }
  v,err:=strconv.ParseUint(body[start:],base,32); if err!=nil || v>utf8.MaxRune || v>=0xd800 && v<=0xdfff { return entity }; return string(rune(v))
 })
}
func tagAttributes(xml,name string) []map[string]string {
 re:=regexp.MustCompile(`(?i)<(?:[\w-]+:)?`+regexp.QuoteMeta(name)+`\b([^>]*)>`)
 out:=make([]map[string]string,0)
 for _,tag:=range re.FindAllStringSubmatch(xml,-1) {
  attrs:=map[string]string{}
  for _,m:=range attributePattern.FindAllStringSubmatch(tag[1],-1) { value:=m[2]; if m[3]!="" { value=m[3] }; attrs[strings.ToLower(m[1])]=decodeEntities(value) }
  out=append(out,attrs)
 }
 return out
}
func firstAttributes(xml,name string) map[string]string { all:=tagAttributes(xml,name); if len(all)==0 { return nil }; return all[0] }
func htmlText(s string) string {
 s=commentPattern.ReplaceAllString(s,""); s=scriptStylePattern.ReplaceAllString(s,""); s=brPattern.ReplaceAllString(s,"\n"); s=blockPattern.ReplaceAllString(s,"\n\n"); s=tagPattern.ReplaceAllString(s,""); s=decodeEntities(s)
 s=strings.ReplaceAll(strings.ReplaceAll(s,"\r\n","\n"),"\r","\n")
 lines:=strings.Split(s,"\n"); for i,line:=range lines { lines[i]=trim(horizontalPattern.ReplaceAllString(line," ")) }
 filtered:=make([]string,0,len(lines)); for i,line:=range lines { if line!="" || i>0 && lines[i-1]!="" { filtered=append(filtered,line) } }
 return trim(newlinesPattern.ReplaceAllString(strings.Join(filtered,"\n"),"\n\n"))
}
func decodeDocument(raw []byte) (string,error) {
 little,big:=false,false
 if len(raw)>=2 { little=raw[0]==0xff && raw[1]==0xfe; big=raw[0]==0xfe && raw[1]==0xff }
 if len(raw)>=4 { little=little || bytes.Equal(raw[:4],[]byte{'<',0,'?',0}); big=big || bytes.Equal(raw[:4],[]byte{0,'<',0,'?'}) }
 if little || big {
  if len(raw)%2!=0 { return "",epubInvalid() }
  start:=0; if len(raw)>=2 && (bytes.Equal(raw[:2],[]byte{0xff,0xfe}) || bytes.Equal(raw[:2],[]byte{0xfe,0xff})) { start=2 }
  units:=make([]uint16,0,(len(raw)-start)/2)
  for i:=start; i<len(raw); i+=2 { v:=uint16(raw[i])<<8|uint16(raw[i+1]); if little { v=uint16(raw[i+1])<<8|uint16(raw[i]) }; units=append(units,v) }
  for i:=0; i<len(units); i++ { v:=units[i]; if v>=0xd800 && v<=0xdbff { if i+1>=len(units) || units[i+1]<0xdc00 || units[i+1]>0xdfff { return "",epubInvalid() }; i++ } else if v>=0xdc00 && v<=0xdfff { return "",epubInvalid() } }
  return string(utf16.Decode(units)),nil
 }
 if !utf8.Valid(raw) { return "",epubInvalid() }; return strings.TrimPrefix(string(raw),"\ufeff"),nil
}

func extractEPUB(raw []byte,maxBytes int64) ([]epubChapter,error) {
 if len(raw)==0 { return nil,epubInvalid() }
 z,err:=zip.NewReader(bytes.NewReader(raw),int64(len(raw))); if err!=nil { return nil,epubInvalid() }
 if len(z.File)>EPUBMaxEntries { return nil,epubExpansion() }
 entries:=make(map[string]*zip.File,len(z.File))
 for _,f:=range z.File {
  name,e:=archivePath(f.Name); if e!=nil { return nil,e }
  if f.Flags&1!=0 || f.Flags&0x40!=0 { return nil,epubDRM() }
  if _,exists:=entries[name]; exists { return nil,epubInvalid() }
  // Reject symlinks, devices, unsupported compression and ZIP bombs up front.
  if !f.Mode().IsRegular() && !f.FileInfo().IsDir() { return nil,epubInvalid() }
  if f.Method!=zip.Store && f.Method!=zip.Deflate { return nil,epubInvalid() }
  if f.UncompressedSize64>uint64(EPUBMaxExtractedBytes) { return nil,epubExpansion() }
  entries[name]=f
 }
 maxEntry:=int64(EPUBMaxEntryBytes); if maxBytes<maxEntry { maxEntry=maxBytes }; totalLimit:=int64(EPUBMaxExtractedBytes); if maxBytes<totalLimit { totalLimit=maxBytes }; consumed:=int64(0)
 read:=func(name string) (string,error) {
  f,ok:=entries[name]; if !ok { return "",epubInvalid() }
  available:=totalLimit-consumed; if available>maxEntry { available=maxEntry }
  if f.UncompressedSize64>uint64(available) { return "",epubExpansion() }
  r,e:=f.Open(); if e!=nil { return "",epubInvalid() }
  data,e:=io.ReadAll(io.LimitReader(r,available+1)); closeError:=r.Close()
  if int64(len(data))>available { return "",epubExpansion() }
  if e!=nil || closeError!=nil || uint64(len(data))!=f.UncompressedSize64 { return "",epubInvalid() }
  consumed+=int64(len(data)); return decodeDocument(data)
 }
 container,err:=read("META-INF/container.xml"); if err!=nil { return nil,err }
 packagePath,err:=archivePath(firstAttributes(container,"rootfile")["full-path"]); if err!=nil { return nil,err }
 opf,err:=read(packagePath); if err!=nil { return nil,err }; directory:=path.Dir(packagePath)
 manifest:=map[string]epubManifest{}; byPath:=map[string]epubManifest{}
 for _,a:=range tagAttributes(opf,"item") {
  if a["id"]=="" || a["href"]=="" { return nil,epubInvalid() }; if _,exists:=manifest[a["id"]]; exists { return nil,epubInvalid() }
  p,e:=referencePath(directory,a["href"]); if e!=nil { return nil,e }; if _,exists:=byPath[p]; exists { return nil,epubInvalid() }
  item:=epubManifest{a["href"],a["media-type"],a["properties"]}; manifest[a["id"]]=item; byPath[p]=item
 }
 if _,exists:=entries["META-INF/encryption.xml"]; exists {
  xml,e:=read("META-INF/encryption.xml"); if e!=nil { return nil,e }
  blocks:=encryptedBlockPattern.FindAllStringSubmatch(xml,-1); if len(blocks)==0 || len(blocks)!=len(encryptedStartPattern.FindAllString(xml,-1)) { return nil,epubInvalid() }
  fonts:=map[string]bool{"application/font-sfnt":true,"application/font-woff":true,"application/vnd.ms-fontobject":true,"application/vnd.ms-opentype":true,"application/x-font-opentype":true,"application/x-font-ttf":true}
  for _,b:=range blocks {
   algorithm:=trim(firstAttributes(b[1],"EncryptionMethod")["algorithm"]); uri:=trim(firstAttributes(b[1],"CipherReference")["uri"])
   if algorithm=="" || uri=="" { return nil,epubInvalid() }
   p,e:=referencePath(".",uri); if e!=nil { return nil,e }; item,exists:=byPath[p]; media:=strings.ToLower(trim(item.mediaType)); _,present:=entries[p]
   supported:=algorithm=="http://www.idpf.org/2008/embedding" || algorithm=="http://ns.adobe.com/pdf/enc#RC"
   if !supported || !exists || !present || !(strings.HasPrefix(media,"font/") || fonts[media]) { return nil,epubDRM() }
  }
 }
 chapters:=make([]epubChapter,0)
 for _,ref:=range tagAttributes(opf,"itemref") {
  if strings.EqualFold(ref["linear"],"no") { continue }; item,exists:=manifest[ref["idref"]]; if !exists { return nil,epubInvalid() }
  media:=strings.ToLower(item.mediaType); if media!="application/xhtml+xml" && media!="text/html" { continue }
  nav:=false; for _,property:=range strings.Fields(item.properties) { if property=="nav" { nav=true } }; if nav { continue }
  p,e:=referencePath(directory,item.href); if e!=nil { return nil,e }; document,e:=read(p); if e!=nil { return nil,e }
  body:=document; if m:=bodyPattern.FindStringSubmatch(document); m!=nil { body=m[1] }; content:=htmlText(body); if content=="" { continue }
  title:=""; if m:=headingPattern.FindStringSubmatch(document); m!=nil { title=htmlText(m[1]) } else if m:=titlePattern.FindStringSubmatch(document); m!=nil { title=htmlText(m[1]) }
  if title=="" { title=strings.TrimSuffix(path.Base(p),path.Ext(p)) }
  chapters=append(chapters,epubChapter{title,content}); if len(chapters)>MaxChapters { return nil,errors.New("IMPORT_CHAPTER_COUNT_EXCEEDED") }
 }
 if len(chapters)==0 { return nil,epubInvalid() }; return chapters,nil
}

// EPUBPreview is a pure convenience for differential tests. It still uses the
// same bounded ZIP implementation as production inspection, never an extractor.
func EPUBPreview(raw []byte) ([]map[string]string,error) {
 chapters,err:=extractEPUB(raw,MaxTotalBytes); if err!=nil { return nil,err }
 out:=make([]map[string]string,0,len(chapters)); for _,c:=range chapters { out=append(out,map[string]string{"title":c.title,"content":c.content}) }; return out,nil
}

