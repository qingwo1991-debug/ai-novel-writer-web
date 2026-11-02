package projectfs

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "io"
 "io/fs"
 "os"
 "path/filepath"
 "sort"
 "strings"
 "sync"
)

var ErrKnowledgeLegacyPending = errors.New("KNOWLEDGE_LEGACY_PENDING")
var ErrKnowledgeCleanup = errors.New("KNOWLEDGE_CLEANUP_FAILED")

// KnowledgeBudget bounds bytes and discovered filesystem entries, not Lance/Arrow
// decoded memory. All fields are required. The hard ceilings are 1 TiB, 10000
// entries and 64 levels; no allocation is based on an unbounded directory listing.
type KnowledgeBudget struct { MaxBytes int64; MaxEntries int; MaxDepth int }
type KnowledgeSummary struct {
 Bytes int64 `json:"bytes"`
 Entries int `json:"entries"`
 Digest string `json:"digest"`
 LanceDB bool `json:"lancedb"`
 EmbeddingRegistry bool `json:"embeddingRegistry"`
 KnowledgeCopies bool `json:"knowledgeCopies"`
 LegacyReceipt bool `json:"legacyReceipt"`
}
type knowledgeEntry struct { Path string `json:"path"`; Kind string `json:"kind"`; Size int64 `json:"size"`; Hash string `json:"hash"`; info fs.FileInfo }
type knowledgeManifest struct { entries []knowledgeEntry; rootInfo fs.FileInfo; bytes int64; count int }
type knowledgeOwned struct { parent *os.Root; name string; info fs.FileInfo; child *os.Root; file *os.File }
// KnowledgeProof borrows Project/Library, but owns a separately opened destination
// descriptor and the descriptors needed for safe cleanup. The supplied target may
// be closed after staging. Always call Cleanup. VerifyCopy needs the original
// physical target anchors unchanged; Cleanup works after target/ancestor rename.
// Each copied file/directory retains a descriptor until Cleanup, preventing
// inode-reuse ownership confusion; MaxEntries must fit caller fd resources.
// No prose, native database handle or author write authority is returned.
type KnowledgeProof struct {
 mu sync.Mutex
 source *Project
 target *os.Root
 targetPath string
 targetInfo fs.FileInfo
 budget KnowledgeBudget
 original, copied knowledgeManifest
 summary KnowledgeSummary
 owned []knowledgeOwned
 cleaned bool
 cleanupErr error
}
var knowledgeAssets = []struct{name string; dir bool}{ {"lancedb",true},{"embedding-spaces.json",false},{"knowledge-copies",true},{"vectors.json.migrated",false} }

func validKnowledgeBudget(b KnowledgeBudget) bool { return b.MaxBytes>0 && b.MaxBytes<=1<<40 && b.MaxEntries>0 && b.MaxEntries<=10000 && b.MaxDepth>0 && b.MaxDepth<=64 }
func knowledgeName(name string) bool { return name!="." && name!=".." && len(name)>0 && len(name)<=255 && !strings.ContainsAny(name,"/\\\x00") && fs.ValidPath(name) }
func knowledgeOverlap(a,b string) bool { r,err:=filepath.Rel(a,b);return err!=nil || r=="." || r!=".." && !strings.HasPrefix(r,".."+string(filepath.Separator)) }
func knowledgeLegacy(root *os.Root)error { for _,name:=range []string{"vectors.json","vectors.json.migration-journal.json"} { if _,err:=root.Lstat(name);err==nil{return ErrKnowledgeLegacyPending}else if !errors.Is(err,fs.ErrNotExist){return err} };return nil }

// StageCanonicalKnowledge makes only physical byte copies; it never opens Lance,
// SQLite or JSON content. PRECONDITION: trusted caller must continuously exclude
// source writers (including old desktop) and all same-user filesystem mutation
// through staging, copy validation, both proof checks and cleanup. Sampling cannot
// provide an atomic multi-table snapshot or defeat swap-away-and-back attacks.
// target must be trusted existing empty private owner directory outside the entire
// source library, with physical non-symlink anchors. Root.Name is checked against
// a freshly physical-opened descriptor and is never trusted by itself.
func (p *Project) StageCanonicalKnowledge(ctx context.Context,target *os.Root,budget KnowledgeBudget)(proof *KnowledgeProof,err error) {
 if ctx==nil || target==nil{return nil,ErrInvalidPath};if err:=ctx.Err();err!=nil{return nil,err};if p==nil||p.library==nil{return nil,ErrClosed};if !platformSupported(){return nil,ErrUnsupported};if !validKnowledgeBudget(budget){return nil,ErrTooLarge}
 p.mu.Lock();defer p.mu.Unlock();p.library.mu.Lock();defer p.library.mu.Unlock()
 if err:=p.checkIdentityLocked();err!=nil{return nil,err}
 path:=target.Name();if !canonicalAbsolute(path)||knowledgeOverlap(p.library.path,path)||knowledgeOverlap(path,p.library.path){return nil,ErrUnsafeDirectory}
 physical,info,err:=openPhysicalRoot(path);if err!=nil{return nil,err};defer physical.Close()
 pinned,err:=target.OpenRoot(".");if err!=nil{return nil,err};opened,err:=pinned.Stat(".");if err!=nil{pinned.Close();return nil,err}
 if !directory(opened)||!knowledgePrivate(opened)||!os.SameFile(info,opened){pinned.Close();return nil,ErrUnsafeDirectory}
 proof=&KnowledgeProof{source:p,target:pinned,targetPath:path,targetInfo:opened,budget:budget}
 ownedProof:=proof
 defer func(){if err!=nil{err=errors.Join(err,ownedProof.Cleanup());proof=nil}}()
 // Refuse preexisting content independently of source membership.
 f,openErr:=pinned.OpenFile(".",readFlags(),0);if openErr!=nil{return nil,openErr};names,readErr:=f.ReadDir(1);closeErr:=f.Close();if len(names)!=0{return nil,fs.ErrExist};if readErr!=nil&&!errors.Is(readErr,io.EOF){return nil,readErr};if closeErr!=nil{return nil,closeErr}
 buffer:=make([]byte,snapshotStreamBuffer)
 before,err:=knowledgeScan(ctx,p.storage,budget,buffer,nil,false);if err!=nil{return nil,err}
 copied,err:=knowledgeScan(ctx,p.storage,budget,buffer,proof,false);if err!=nil{return nil,err};if !knowledgeSame(before,copied,true){return nil,ErrChanged}
 after,err:=knowledgeScan(ctx,p.storage,budget,buffer,nil,false);if err!=nil{return nil,err};if !knowledgeSame(before,after,true){return nil,ErrChanged}
 dest,err:=knowledgeScan(ctx,pinned,budget,buffer,nil,true);if err!=nil{return nil,err};if !knowledgeSame(before,dest,false){return nil,ErrChanged}
 if err:=p.checkIdentityLocked();err!=nil{return nil,err};if err:=proof.checkTarget();err!=nil{return nil,err};if err:=ctx.Err();err!=nil{return nil,err}
 proof.original,proof.copied=before,dest;proof.targetInfo=dest.rootInfo
 raw,_:=json.Marshal(before.entries);digest:=sha256.Sum256(raw);proof.summary=KnowledgeSummary{Bytes:before.bytes,Entries:before.count,Digest:hex.EncodeToString(digest[:])}
 for _,entry:=range before.entries {if entry.Kind=="absent"{continue};switch entry.Path{case "lancedb":proof.summary.LanceDB=true;case "embedding-spaces.json":proof.summary.EmbeddingRegistry=true;case "knowledge-copies":proof.summary.KnowledgeCopies=true;case "vectors.json.migrated":proof.summary.LegacyReceipt=true}}
 return proof,nil
}

// knowledgeNames limits each ReadDir to <=128. The extra sentinel entry is never
// retained: it proves overflow before appending beyond the caller's remaining
// entry budget. File.ReadDir's bounded batch allocates at most 128 DirEntry values.
func knowledgeNames(ctx context.Context,root *os.Root,limit int)([]string,error) {
 file,err:=root.OpenFile(".",readFlags(),0);if err!=nil{return nil,err};defer file.Close();before,err:=file.Stat();if err!=nil{return nil,err};var names []string
 for {if err:=ctx.Err();err!=nil{return nil,err};n:=limit-len(names)+1;if n>128{n=128};if n<1{n=1};batch,err:=file.ReadDir(n);if len(batch)>limit-len(names){return nil,ErrTooLarge};for _,entry:=range batch {if !knowledgeName(entry.Name()){return nil,ErrInvalidPath};names=append(names,entry.Name())};if errors.Is(err,io.EOF){break};if err!=nil{return nil,err};if len(batch)==0{return nil,io.ErrNoProgress}}
 after,err:=file.Stat();if err!=nil||!sameAssetEntry(before,after){return nil,ErrChanged};named,err:=root.Stat(".");if err!=nil||!sameAssetEntry(after,named){return nil,ErrChanged};sort.Strings(names);return names,nil
}

type knowledgeScanner struct {ctx context.Context;budget KnowledgeBudget;buffer []byte;manifest knowledgeManifest;destination *KnowledgeProof;copySide bool}
func knowledgeScan(ctx context.Context,root *os.Root,budget KnowledgeBudget,buffer []byte,dest *KnowledgeProof,copySide bool)(knowledgeManifest,error) {
 s:=knowledgeScanner{ctx:ctx,budget:budget,buffer:buffer,destination:dest,copySide:copySide};before,err:=root.Stat(".");if err!=nil{return s.manifest,err};if err:=knowledgeLegacy(root);err!=nil{return s.manifest,err}
 if copySide {names,err:=knowledgeNames(ctx,root,budget.MaxEntries);if err!=nil{return s.manifest,err};for _,name:=range names{found:=false;for _,asset:=range knowledgeAssets{if name==asset.name{found=true;break}};if !found{return s.manifest,ErrChanged}}}
 for _,asset:=range knowledgeAssets {if err:=ctx.Err();err!=nil{return s.manifest,err};info,err:=root.Lstat(asset.name);if errors.Is(err,fs.ErrNotExist){s.manifest.entries=append(s.manifest.entries,knowledgeEntry{Path:asset.name,Kind:"absent"});continue};if err!=nil{return s.manifest,err};if asset.dir!=info.IsDir(){return s.manifest,ErrUnsafeFile};if err:=s.reserve(1);err!=nil{return s.manifest,err};if err:=s.node(root,asset.name,asset.name,1,info,destRoot(dest));err!=nil{return s.manifest,err}}
 if err:=knowledgeLegacy(root);err!=nil{return s.manifest,err};after,err:=root.Stat(".");if err!=nil||!sameAssetEntry(before,after){return s.manifest,ErrChanged};s.manifest.rootInfo=after;sort.Slice(s.manifest.entries,func(i,j int)bool{return s.manifest.entries[i].Path<s.manifest.entries[j].Path});return s.manifest,ctx.Err()
}
func destRoot(proof *KnowledgeProof)*os.Root{if proof==nil{return nil};return proof.target}
func(s *knowledgeScanner)reserve(n int)error{if n<0||n>s.budget.MaxEntries-s.manifest.count{return ErrTooLarge};s.manifest.count+=n;return nil}
func(s *knowledgeScanner)node(parent *os.Root,name,path string,depth int,before fs.FileInfo,destParent *os.Root)error{
 if err:=s.ctx.Err();err!=nil{return err};if depth>s.budget.MaxDepth||len(path)>4096{return ErrTooLarge};if before.Mode()&os.ModeSymlink!=0{return ErrUnsafeFile}
 if before.IsDir(){child,opened,err:=openChild(parent,name);if err!=nil{return err};defer child.Close();if !sameAssetEntry(before,opened){return ErrChanged};if s.copySide&&!knowledgePrivate(opened){return ErrUnsafeDirectory};var destChild *os.Root
  if destParent!=nil {if err:=destParent.Mkdir(name,0700);err!=nil{return err};info,err:=destParent.Lstat(name);owned:=knowledgeOwned{parent:destParent,name:name,info:info};s.destination.owned=append(s.destination.owned,owned);if err!=nil{return err};destChild,info,err=openChild(destParent,name);s.destination.owned[len(s.destination.owned)-1].child=destChild;if err!=nil{return err};if !knowledgePrivate(info){return ErrUnsafeDirectory}}
  s.manifest.entries=append(s.manifest.entries,knowledgeEntry{Path:path,Kind:"directory",info:opened});names,err:=knowledgeNames(s.ctx,child,s.budget.MaxEntries-s.manifest.count);if err!=nil{return err};if err:=s.reserve(len(names));err!=nil{return err};for _,childName:=range names {info,err:=child.Lstat(childName);if err!=nil{return err};if err:=s.node(child,childName,path+"/"+childName,depth+1,info,destChild);err!=nil{return err}}
  after,err:=child.Stat(".");named,nameErr:=parent.Lstat(name);if err!=nil||nameErr!=nil||!sameAssetEntry(opened,after)||!sameAssetEntry(after,named){return ErrChanged};return nil
 }
 file,opened,err:=openRegular(parent,name);if err!=nil{return err};defer file.Close();if !sameVersion(before,opened){return ErrChanged};if s.copySide&&!knowledgePrivateFile(opened){return ErrUnsafeFile};if opened.Size()<0||opened.Size()>s.budget.MaxBytes-s.manifest.bytes{return ErrTooLarge};var output *os.File
 if destParent!=nil {output,err=destParent.OpenFile(name,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600);if err!=nil{return err};info,statErr:=output.Stat();s.destination.owned=append(s.destination.owned,knowledgeOwned{parent:destParent,name:name,info:info,file:output});if statErr!=nil{return statErr};if err:=output.Chmod(0600);err!=nil{return err};if !info.Mode().IsRegular()||!singleLink(info){return ErrUnsafeFile}}
 hash:=sha256.New();var count int64
 for {if err:=s.ctx.Err();err!=nil{return err};chunk:=s.buffer;if remaining:=opened.Size()-count;remaining<int64(len(chunk)){chunk=chunk[:int(remaining)+1]};n,readErr:=file.Read(chunk);if int64(n)>opened.Size()-count{return ErrChanged};if err:=s.ctx.Err();err!=nil{return err};if n>0{hash.Write(chunk[:n]);if output!=nil{written,err:=output.Write(chunk[:n]);if err!=nil{return err};if written!=n{return io.ErrShortWrite}};count+=int64(n)};if errors.Is(readErr,io.EOF){break};if readErr!=nil{return readErr};if n==0{return io.ErrNoProgress}}
 after,err:=file.Stat();named,nameErr:=parent.Lstat(name);if err!=nil||nameErr!=nil||!sameVersion(opened,after)||!sameVersion(after,named)||count!=opened.Size(){return ErrChanged};// Keep the exclusively created file descriptor pinned until Cleanup, preventing
 // inode-number reuse from making a same-bytes replacement look owned.
 s.manifest.bytes+=count;s.manifest.entries=append(s.manifest.entries,knowledgeEntry{Path:path,Kind:"file",Size:count,Hash:hex.EncodeToString(hash.Sum(nil)),info:after});return nil
}
func knowledgeSame(a,b knowledgeManifest,physical bool)bool{
 if a.bytes!=b.bytes||a.count!=b.count||len(a.entries)!=len(b.entries){return false};if physical&&!sameAssetEntry(a.rootInfo,b.rootInfo){return false};for i,x:=range a.entries{y:=b.entries[i];if x.Path!=y.Path||x.Kind!=y.Kind||x.Size!=y.Size||x.Hash!=y.Hash{return false};if physical&&x.Kind!="absent"&&!sameAssetEntry(x.info,y.info){return false}};return true
}
func(proof *KnowledgeProof)checkTarget()error{root,info,err:=openPhysicalRoot(proof.targetPath);if err!=nil{return err};root.Close();pinned,err:=proof.target.Stat(".");if err!=nil||!knowledgePrivate(pinned)||!os.SameFile(info,pinned)||!os.SameFile(pinned,proof.targetInfo){return ErrChanged};return nil}
func(proof *KnowledgeProof)Summary()KnowledgeSummary{if proof==nil{return KnowledgeSummary{}};proof.mu.Lock();defer proof.mu.Unlock();return proof.summary}
func(proof *KnowledgeProof)VerifyUnchanged(ctx context.Context)error{
 if ctx==nil{return ErrInvalidPath};if proof==nil{return ErrClosed};proof.mu.Lock();defer proof.mu.Unlock();if proof.cleaned||proof.source==nil{return ErrClosed};p:=proof.source;p.mu.Lock();defer p.mu.Unlock();p.library.mu.Lock();defer p.library.mu.Unlock();if err:=p.checkIdentityLocked();err!=nil{return err};now,err:=knowledgeScan(ctx,p.storage,proof.budget,make([]byte,snapshotStreamBuffer),nil,false);if err!=nil{return err};if !knowledgeSame(proof.original,now,true){return ErrChanged};if err:=p.checkIdentityLocked();err!=nil{return err};return ctx.Err()
}
func(proof *KnowledgeProof)VerifyCopy(ctx context.Context)error{if ctx==nil{return ErrInvalidPath};if proof==nil{return ErrClosed};proof.mu.Lock();defer proof.mu.Unlock();if proof.cleaned||proof.target==nil{return ErrClosed};if err:=proof.checkTarget();err!=nil{return err};now,err:=knowledgeScan(ctx,proof.target,proof.budget,make([]byte,snapshotStreamBuffer),nil,true);if err!=nil{return err};if !knowledgeSame(proof.copied,now,true){return ErrChanged};if err:=proof.checkTarget();err!=nil{return err};return ctx.Err()}
// Cleanup never follows current host paths and never uses RemoveAll. It removes
// only same-inode single-link owned objects, children before parents; unknown or
// replacement entries survive. Missing owned entries are tolerated. All retained
// descriptors are closed even on failure; repeated calls return the same result.
func(proof *KnowledgeProof)Cleanup()error{if proof==nil{return nil};proof.mu.Lock();defer proof.mu.Unlock();if proof.cleaned{return proof.cleanupErr};var result error
 for i:=len(proof.owned)-1;i>=0;i-- {owned:=proof.owned[i];named,err:=owned.parent.Lstat(owned.name);if errors.Is(err,fs.ErrNotExist){continue};if err!=nil{result=errors.Join(result,err);continue};if owned.info==nil||!os.SameFile(owned.info,named)||owned.info.IsDir()!=named.IsDir()||!named.IsDir()&&!singleLink(named){result=errors.Join(result,ErrChanged);continue};result=errors.Join(result,owned.parent.Remove(owned.name))}
 if proof.target!=nil{names,err:=knowledgeNames(context.Background(),proof.target,0);if err!=nil||len(names)!=0{result=errors.Join(result,ErrChanged,err)}}
 for i:=len(proof.owned)-1;i>=0;i--{if proof.owned[i].file!=nil{result=errors.Join(result,proof.owned[i].file.Close())};if proof.owned[i].child!=nil{result=errors.Join(result,proof.owned[i].child.Close())}};if proof.target!=nil{result=errors.Join(result,proof.target.Close())};proof.cleaned=true;if result!=nil{proof.cleanupErr=errors.Join(ErrKnowledgeCleanup,result)};return proof.cleanupErr
}
