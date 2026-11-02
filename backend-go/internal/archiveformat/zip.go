package archiveformat

import (
 "bytes"
 "crypto/sha256"
 "encoding/binary"
 "encoding/hex"
 "errors"
 "hash/crc32"
 "io"
 "math"
 "sort"
 "unicode/utf8"
)

const manifestName = "manifest.json"
const uint32Max = uint64(0xffffffff)
const uint16Max = uint64(0xffff)
const regularMode = uint32(0100600 << 16)
var le = binary.LittleEndian

type Receipt struct { EntryCount int `json:"entryCount"`; PayloadBytes uint64 `json:"payloadBytes"`; ArchiveBytes uint64 `json:"archiveBytes"`; ManifestSHA256 string `json:"manifestSha256"` }
type Source struct { Entry Entry; Reader io.Reader }
// Stage is an internal private staging capability. Open must CREATE_NEW under a
// private owned root without following links. Complete seals staging, NOT a
// publication into an author project. Abort must remove only owned objects.
// A future restoration owner must provide these guarantees and publication.
type Stage interface { Open(Entry) (io.WriteCloser,error); Complete(Manifest,Receipt) error; Abort() error }
type record struct { name string; size,compressed,offset,data,end uint64; crc uint32; flags uint16 }
type Archive struct { reader io.ReaderAt; size int64; limits Limits; manifest Manifest; manifestBytes []byte; receipt Receipt; records []record }
func (a *Archive) Manifest() Manifest { b,_:=marshalJS(a.manifest); m,_:=ParseManifest(b,a.limits); return m }
func (a *Archive) Receipt() Receipt { return a.receipt }
func invalid() error { return failure("PORTABLE_ARCHIVE_INVALID") }
func readAt(r io.ReaderAt, offset uint64, size uint64, bound uint64) ([]byte,error) {
 if offset>bound || size>bound-offset || size>uint64(math.MaxInt) { return nil,invalid() }; b:=make([]byte,int(size)); n,e:=r.ReadAt(b,int64(offset)); if e!=nil && e!=io.EOF { return nil,invalid() }; if n!=len(b) { return nil,invalid() }; return b,nil
}
func exactExtra(extra []byte, sizes []uint64) error {
 if len(sizes)==0 { if len(extra)!=0 { return invalid() }; return nil }
 if len(extra)!=4+8*len(sizes) || le.Uint16(extra)!=1 || int(le.Uint16(extra[2:]))!=8*len(sizes) { return invalid() }
 for i,v:=range sizes { if le.Uint64(extra[4+8*i:])!=v { return invalid() } }; return nil
}
// Inspect validates all metadata before streaming bounded CRC/SHA-256 checks.
// Only the deliberately stored-only portable subset is accepted, not arbitrary
// ZIP compression or filesystem metadata. Memory is O(manifest + entry count).
func Inspect(r io.ReaderAt, size int64, configured Limits) (*Archive,error) {
 l,e:=configured.resolved(); if e!=nil { return nil,e }; if r==nil || size<22 { return nil,invalid() }; bound:=uint64(size)
 end,e:=readAt(r,bound-22,22,bound); if e!=nil || le.Uint32(end)!=0x06054b50 || le.Uint16(end[4:])!=0 || le.Uint16(end[6:])!=0 || le.Uint16(end[20:])!=0 { return nil,invalid() }
 count:=uint64(le.Uint16(end[10:])); centralSize:=uint64(le.Uint32(end[12:])); centralOffset:=uint64(le.Uint32(end[16:])); centralEnd:=bound-22
 if le.Uint16(end[8:])!=le.Uint16(end[10:]) { return nil,invalid() }
 zip64:=count==uint16Max || centralSize==uint32Max || centralOffset==uint32Max
 if zip64 {
  if bound<98 { return nil,invalid() }; locator,e:=readAt(r,bound-42,20,bound); if e!=nil || le.Uint32(locator)!=0x07064b50 || le.Uint32(locator[4:])!=0 || le.Uint32(locator[16:])!=1 { return nil,invalid() }
  off:=le.Uint64(locator[8:]); z,e:=readAt(r,off,56,bound); if e!=nil || off!=bound-98 || le.Uint32(z)!=0x06064b50 || le.Uint64(z[4:])!=44 || le.Uint16(z[14:])>45 || le.Uint32(z[16:])!=0 || le.Uint32(z[20:])!=0 || le.Uint64(z[24:])!=le.Uint64(z[32:]) { return nil,invalid() }
  actualCount,actualSize,actualOffset:=le.Uint64(z[32:]),le.Uint64(z[40:]),le.Uint64(z[48:])
  if (count!=uint16Max && count!=actualCount) || (centralSize!=uint32Max && centralSize!=actualSize) || (centralOffset!=uint32Max && centralOffset!=actualOffset) { return nil,invalid() }
  count,centralSize,centralOffset=actualCount,actualSize,actualOffset; centralEnd=off
 }
 if count==0 || count>l.MaxEntries+1 || count>uint64(math.MaxInt) || centralOffset>centralEnd || centralSize!=centralEnd-centralOffset || count>centralSize/46 { return nil,invalid() }
 a:=&Archive{reader:r,size:size,limits:l,records:make([]record,0,int(count))}; cursor:=centralOffset; seen:=map[string]bool{}; var total,compressed uint64
 for i:=uint64(0);i<count;i++ {
  h,e:=readAt(r,cursor,46,centralEnd); if e!=nil || le.Uint32(h)!=0x02014b50 { return nil,invalid() }
  flags:=le.Uint16(h[8:]); method:=le.Uint16(h[10:]); nameLen,extraLen,commentLen:=uint64(le.Uint16(h[28:])),uint64(le.Uint16(h[30:])),uint64(le.Uint16(h[32:])); attr:=le.Uint32(h[38:]); creator:=h[5]
  if flags&0x800==0 || flags & ^uint16(0x808)!=0 || method!=0 || nameLen==0 || nameLen>l.MaxPathBytes || commentLen!=0 || le.Uint16(h[34:])!=0 || le.Uint16(h[36:])!=0 || attr&0x410!=0 || (creator==3 && (attr>>16)&0xf000!=0x8000) || le.Uint16(h[6:])>45 { return nil,invalid() }
  variable,e:=readAt(r,cursor+46,nameLen+extraLen,centralEnd); if e!=nil { return nil,e }; name:=string(variable[:nameLen]); if !utf8.ValidString(name) { return nil,invalid() }; extra:=variable[nameLen:]
  rec:=record{name:name,size:uint64(le.Uint32(h[24:])),compressed:uint64(le.Uint32(h[20:])),offset:uint64(le.Uint32(h[42:])),crc:le.Uint32(h[16:]),flags:flags}
  needs:=[]*uint64{}; if rec.size==uint32Max { needs=append(needs,&rec.size) }; if rec.compressed==uint32Max { needs=append(needs,&rec.compressed) }; if rec.offset==uint32Max { needs=append(needs,&rec.offset) }
  if len(needs)>0 { if len(extra)!=4+len(needs)*8 || le.Uint16(extra)!=1 || int(le.Uint16(extra[2:]))!=len(needs)*8 { return nil,invalid() }; for j,p:=range needs { *p=le.Uint64(extra[4+j*8:]); if *p<uint32Max || *p>maxSafeInteger { return nil,invalid() } } } else if len(extra)!=0 { return nil,invalid() }
  if rec.size!=rec.compressed || rec.size>maxSafeInteger || rec.offset>=centralOffset { return nil,invalid() }
  if i==0 { if name!=manifestName { return nil,invalid() }; if rec.size>l.MaxEntryBytes || rec.size>MaxManifestBytes { return nil,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") } } else {
   key,e:=PathKey(name,int(l.MaxPathBytes)); if e!=nil { return nil,invalid() }; if key==manifestName || seen[key] { return nil,invalid() }; seen[key]=true
   if rec.size>l.MaxEntryBytes || rec.compressed>l.MaxDeclaredCompressedBytes || ratioExceeded(rec.size,rec.compressed,l.MaxCompressionRatio) || rec.size>l.MaxTotalBytes-total || rec.compressed>l.MaxDeclaredCompressedBytes-compressed { return nil,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED") }; total+=rec.size; compressed+=rec.compressed
  }
  local,e:=readAt(r,rec.offset,30,centralOffset); if e!=nil || le.Uint32(local)!=0x04034b50 || le.Uint16(local[4:])>45 || le.Uint16(local[6:])!=flags || le.Uint16(local[8:])!=method || uint64(le.Uint16(local[26:]))!=nameLen { return nil,invalid() }
  localExtraLen:=uint64(le.Uint16(local[28:])); lv,e:=readAt(r,rec.offset+30,nameLen+localExtraLen,centralOffset); if e!=nil || !bytes.Equal(lv[:nameLen],[]byte(name)) { return nil,invalid() }
  localSizes:=[]uint64{}; if rec.size>=uint32Max { localSizes=[]uint64{rec.size,rec.compressed} }; if e=exactExtra(lv[nameLen:],localSizes); e!=nil { return nil,e }
  lc,ls,lcrc:=uint64(le.Uint32(local[18:])),uint64(le.Uint32(local[22:])),le.Uint32(local[14:]); if rec.size>=uint32Max { if lc!=uint32Max || ls!=uint32Max { return nil,invalid() } } else if flags&8==0 { if lc!=rec.compressed || ls!=rec.size { return nil,invalid() } } else if !((lc==0 && ls==0) || (lc==rec.compressed && ls==rec.size)) { return nil,invalid() }
  if (flags&8==0 && lcrc!=rec.crc) || (flags&8!=0 && lcrc!=0 && lcrc!=rec.crc) { return nil,invalid() }
  rec.data=rec.offset+30+nameLen+localExtraLen; if rec.data>centralOffset || rec.size>centralOffset-rec.data { return nil,invalid() }; rec.end=rec.data+rec.size
  if flags&8!=0 {
   // Both signed and unsigned descriptors are ZIP-standard. Native output
   // matches the signed descriptor used by the original JS implementation.
   first,e:=readAt(r,rec.end,4,centralOffset); if e!=nil { return nil,e }; signed:=le.Uint32(first)==0x08074b50; n:=uint64(12); if rec.size>=uint32Max { n=20 }; if signed { n+=4 }; desc,e:=readAt(r,rec.end,n,centralOffset); if e!=nil { return nil,e }; if signed { desc=desc[4:] }; if le.Uint32(desc)!=rec.crc { return nil,invalid() }
   if rec.size>=uint32Max { if le.Uint64(desc[4:])!=rec.compressed || le.Uint64(desc[12:])!=rec.size { return nil,invalid() } } else if uint64(le.Uint32(desc[4:]))!=rec.compressed || uint64(le.Uint32(desc[8:]))!=rec.size { return nil,invalid() }; rec.end+=n
  }
  a.records=append(a.records,rec); cursor+=46+nameLen+extraLen
 }
 if cursor!=centralEnd { return nil,invalid() }
 // No preamble, overlapping regions, hidden local entries or unreferenced
 // bytes. This is stronger than yauzl's central-directory-only lookup.
 intervals:=append([]record(nil),a.records...); sort.Slice(intervals,func(i,j int)bool{return intervals[i].offset<intervals[j].offset}); position:=uint64(0)
 for _,rec:=range intervals { if rec.offset!=position { return nil,invalid() }; position=rec.end }; if position!=centralOffset { return nil,invalid() }
 mrec:=a.records[0]; mb,e:=readAt(r,mrec.data,mrec.size,centralOffset); if e!=nil || crc32.ChecksumIEEE(mb)!=mrec.crc { return nil,invalid() }; m,e:=ParseManifest(bytes.TrimPrefix(mb,[]byte{0xef,0xbb,0xbf}),l); if e!=nil { return nil,invalid() }
 if m.DeclaredUncompressedBytes!=total || m.DeclaredCompressedBytes!=compressed || len(m.Entries)!=len(a.records)-1 { return nil,invalid() }
 for i,entry:=range m.Entries { rec:=a.records[i+1]; if entry.Path!=rec.name || entry.ByteSize!=rec.size { return nil,invalid() }; if e=copyVerified(io.Discard,io.NewSectionReader(r,int64(rec.data),int64(rec.size)),entry,rec.crc,false); e!=nil { return nil,e } }
 digest:=sha256.Sum256(mb); a.manifest=m; a.manifestBytes=mb; a.receipt=Receipt{len(m.Entries),total,bound,hex.EncodeToString(digest[:])}; return a,nil
}
func copyVerified(w io.Writer, r io.Reader, entry Entry, crc uint32, source bool) error {
 if r==nil || w==nil { return invalid() }; digest:=sha256.New(); checksum:=crc32.NewIEEE(); n,e:=io.CopyBuffer(io.MultiWriter(w,digest,checksum),io.LimitReader(r,int64(entry.ByteSize)+1),make([]byte,1024*1024))
 if e!=nil { return e }; if uint64(n)!=entry.ByteSize || hex.EncodeToString(digest.Sum(nil))!=entry.SHA256 || (!source && checksum.Sum32()!=crc) { if source { return failure("PORTABLE_ARCHIVE_SOURCE_CHANGED") }; return invalid() }; return nil
}
func (a *Archive) Stage(stage Stage) (err error) {
 if stage==nil { return invalid() }; complete:=false; defer func(){ if !complete { if cleanupErr:=stage.Abort(); cleanupErr!=nil { err=errors.Join(err,failure("ARCHIVE_STAGE_CLEANUP_FAILED")) } } }()
 // Re-index before staging and recheck every payload during staging. ReaderAt
 // is a capability over a stable owner-held snapshot, not an arbitrary path.
 current,e:=Inspect(a.reader,a.size,a.limits); if e!=nil { return failure(Code(e)) }; if current.receipt!=a.receipt || !bytes.Equal(current.manifestBytes,a.manifestBytes) { return failure("PORTABLE_ARCHIVE_SOURCE_CHANGED") }
 for i,entry:=range a.manifest.Entries { rec:=current.records[i+1]; w,e:=stage.Open(entry); if e!=nil || w==nil { if w!=nil { if closeErr:=w.Close(); closeErr!=nil { return errors.Join(failure("ARCHIVE_STAGE_FAILED"),failure("ARCHIVE_STAGE_CLOSE_FAILED")) } }; return failure("ARCHIVE_STAGE_FAILED") }; e=copyVerified(w,io.NewSectionReader(a.reader,int64(rec.data),int64(rec.size)),entry,rec.crc,false); closeErr:=w.Close(); if e!=nil { if closeErr!=nil { return errors.Join(failure(Code(e)),failure("ARCHIVE_STAGE_CLOSE_FAILED")) }; return failure(Code(e)) }; if closeErr!=nil { return failure("ARCHIVE_STAGE_CLOSE_FAILED") } }
 if e=stage.Complete(a.Manifest(),a.receipt); e!=nil { return failure("ARCHIVE_STAGE_FAILED") }; complete=true; return nil
}

type countedWriter struct { w io.Writer; n uint64 }
func (w *countedWriter) Write(p []byte)(int,error) { n,e:=w.w.Write(p); if n<0 || n>len(p) { return 0,io.ErrShortWrite }; w.n+=uint64(n); if e==nil && n!=len(p) { e=io.ErrShortWrite }; return n,e }
func put16(b []byte,o int,v uint64){le.PutUint16(b[o:],uint16(v))}; func put32(b []byte,o int,v uint64){le.PutUint32(b[o:],uint32(v))}; func put64(b []byte,o int,v uint64){le.PutUint64(b[o:],v)}
func writeParts(w io.Writer,parts ...[]byte)error{for _,p:=range parts{if _,e:=w.Write(p);e!=nil{return e}};return nil}
func localHeader(name string,size uint64,crc uint32,descriptor bool)[]byte{
 extra:=[]byte{}; if size>=uint32Max {extra=make([]byte,20);put16(extra,0,1);put16(extra,2,16);put64(extra,4,size);put64(extra,12,size)}
 h:=make([]byte,30+len(name)+len(extra));put32(h,0,0x04034b50);version:=uint64(20);if len(extra)>0{version=45};put16(h,4,version);flags:=uint64(0x800);if descriptor{flags|=8};put16(h,6,flags);if !descriptor{put32(h,14,uint64(crc))};s:=size;if size>=uint32Max{s=uint32Max}else if descriptor{s=0};put32(h,18,s);put32(h,22,s);put16(h,26,uint64(len(name)));put16(h,28,uint64(len(extra)));copy(h[30:],name);copy(h[30+len(name):],extra);return h
}
func centralHeader(rec record)[]byte{
 values:=[]uint64{};if rec.size>=uint32Max{values=append(values,rec.size,rec.size)};if rec.offset>=uint32Max{values=append(values,rec.offset)};extra:=[]byte{};if len(values)>0{extra=make([]byte,4+8*len(values));put16(extra,0,1);put16(extra,2,uint64(8*len(values)));for i,v:=range values{put64(extra,4+i*8,v)}}
 h:=make([]byte,46+len(rec.name)+len(extra));put32(h,0,0x02014b50);version:=uint64(20);if len(extra)>0{version=45};put16(h,4,(3<<8)|version);put16(h,6,version);put16(h,8,uint64(rec.flags));put32(h,16,uint64(rec.crc));s:=rec.size;if s>=uint32Max{s=uint32Max};put32(h,20,s);put32(h,24,s);put16(h,28,uint64(len(rec.name)));put16(h,30,uint64(len(extra)));put32(h,38,uint64(regularMode));o:=rec.offset;if o>=uint32Max{o=uint32Max};put32(h,42,o);copy(h[46:],rec.name);copy(h[46+len(rec.name):],extra);return h
}
func endRecords(count,size,offset uint64,force bool)[]byte{
 zip64:=force||count>=uint16Max||size>=uint32Max||offset>=uint32Max;end:=make([]byte,22);put32(end,0,0x06054b50);if !zip64{put16(end,8,count);put16(end,10,count);put32(end,12,size);put32(end,16,offset);return end};put16(end,8,uint16Max);put16(end,10,uint16Max);put32(end,12,uint32Max);put32(end,16,uint32Max)
 z:=make([]byte,76);put32(z,0,0x06064b50);put64(z,4,44);put16(z,12,45);put16(z,14,45);put64(z,24,count);put64(z,32,count);put64(z,40,size);put64(z,48,offset);put32(z,56,0x07064b50);put64(z,64,offset+size);put32(z,72,1);return append(z,end...)
}
// Build writes an INCOMPLETE staging stream until it returns a valid receipt.
// On ANY error the owner MUST discard the entire stream; this function neither
// publishes nor truncates/renames user paths. Readers must be stable snapshots.
func Build(output io.Writer, input Manifest, sources []Source, configured Limits)(Receipt,error){
 var receipt Receipt;l,e:=configured.resolved();if e!=nil{return receipt,e};mb,e:=input.Bytes(l);if e!=nil{return receipt,e};m,e:=ParseManifest(mb,l);if e!=nil{return receipt,e};if uint64(len(mb))>l.MaxEntryBytes||len(mb)>MaxManifestBytes{return receipt,failure("PORTABLE_ARCHIVE_LIMIT_EXCEEDED")};if m.DeclaredCompressedBytes!=m.DeclaredUncompressedBytes||len(sources)!=len(m.Entries)||output==nil{return receipt,invalid()}
 byKey:=map[string]Source{};for _,source:=range sources{key,e:=PathKey(source.Entry.Path,int(l.MaxPathBytes));if e!=nil{return receipt,e};if _,ok:=byKey[key];ok{return receipt,invalid()};byKey[key]=source};ordered:=make([]Source,0,len(sources));for _,entry:=range m.Entries{key,_:=PathKey(entry.Path,int(l.MaxPathBytes));source,ok:=byKey[key];if key==manifestName||len(entry.Path)>int(uint16Max)||!ok||source.Entry!=entry||source.Reader==nil{return receipt,invalid()};ordered=append(ordered,source)}
 w:=&countedWriter{w:output};records:=make([]record,0,len(sources)+1);checksum:=crc32.ChecksumIEEE(mb);records=append(records,record{name:manifestName,size:uint64(len(mb)),crc:checksum,flags:0x800});if e=writeParts(w,localHeader(manifestName,uint64(len(mb)),checksum,false),mb);e!=nil{return receipt,e}
 for _,source:=range ordered{entry:=source.Entry;offset:=w.n;if e=writeParts(w,localHeader(entry.Path,entry.ByteSize,0,true));e!=nil{return receipt,e};crc:=crc32.NewIEEE();if e=copyVerified(io.MultiWriter(w,crc),source.Reader,entry,0,true);e!=nil{return receipt,e};checksum=crc.Sum32();d:=make([]byte,16);if entry.ByteSize>=uint32Max{d=make([]byte,24)};put32(d,0,0x08074b50);put32(d,4,uint64(checksum));if entry.ByteSize>=uint32Max{put64(d,8,entry.ByteSize);put64(d,16,entry.ByteSize)}else{put32(d,8,entry.ByteSize);put32(d,12,entry.ByteSize)};if e=writeParts(w,d);e!=nil{return receipt,e};records=append(records,record{name:entry.Path,size:entry.ByteSize,offset:offset,crc:checksum,flags:0x808})}
 centralOffset:=w.n;force:=false;for _,rec:=range records{force=force||rec.size>=uint32Max||rec.offset>=uint32Max;if e=writeParts(w,centralHeader(rec));e!=nil{return receipt,e}};centralSize:=w.n-centralOffset;if e=writeParts(w,endRecords(uint64(len(records)),centralSize,centralOffset,force));e!=nil{return receipt,e};digest:=sha256.Sum256(mb);return Receipt{len(m.Entries),m.DeclaredUncompressedBytes,w.n,hex.EncodeToString(digest[:])},nil
}
