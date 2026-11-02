package promptskills

import (
 "bytes"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "io/fs"
 "path/filepath"
 "sort"
 "strings"
 "sync"
 "unicode/utf8"
)

const MaxPromptBytes = 2 * 1024 * 1024

type Service struct { mu sync.Mutex; root *ownedDir; rootPath string; receipts map[receiptKey]receipt; closed bool; authority RemoteAuthority }
var ErrClosed = errors.New("PROMPT_SKILLS_CLOSED")
// New accepts only a trusted, existing, current-UID-owned application root.
// Never pass renderer paths or external-grant paths here. Every ancestor is
// opened and pinned without following symlinks. Close after all requests drain.
func New(root string)(*Service,error) {d,err:=openOwnedRoot(root);if err!=nil{return nil,err};return &Service{root:d,rootPath:root,receipts:map[receiptKey]receipt{}},nil}
func (s *Service) Close() error {s.mu.Lock();defer s.mu.Unlock();if s.closed{return nil};s.closed=true;s.receipts=nil;err:=s.root.close();s.root=nil;return err}
func promptLanguage(language string, optional bool) bool {return language=="zh-CN"||language=="en-US"||(optional&&language=="")}
func promptName(key,language string)(string,error) {if !ValidName(key)||!promptLanguage(language,true){return "",errors.New("The prompt identifier or writing language is invalid")};if language!="" {key+="."+language};return key+".json",nil}
func templateIdentity(template map[string]any)(string,string,error) {if template==nil{return "","",errors.New("The prompt content is invalid")};key,ok:=template["key"].(string);if !ok{return "","",errors.New("The prompt content is invalid")};language:="";if v,exists:=template["writingLanguage"];exists {var ok bool;language,ok=v.(string);if !ok||!promptLanguage(language,false){return "","",errors.New("The prompt content is invalid")}};return key,language,nil}
func (s *Service) LoadPrompts()(map[string]any,error) {
 s.mu.Lock();defer s.mu.Unlock();if s.closed{return nil,ErrClosed};templates:=[]map[string]any{};diagnostics:=[]map[string]any{};result:=map[string]any{"templates":templates,"diagnostics":diagnostics};dir,err:=openChildDir(s.root,"prompts",false,true);if errors.Is(err,fs.ErrNotExist){return result,nil};if err!=nil {return nil,err};defer dir.close();items,err:=entries(dir);if err!=nil {result["diagnostics"]=[]map[string]any{{"path":"prompts","error":"The prompt directory cannot be read"}};return result,nil};sort.Slice(items,func(i,j int)bool{return items[i].Name()<items[j].Name()})
 for _,entry:=range items {name:=entry.Name();if !entry.Type().IsRegular()||!strings.HasSuffix(name,".json") {continue};data,opErr:=readAt(dir,name,MaxPromptBytes);var template map[string]any;if opErr==nil {opErr=json.Unmarshal(data,&template)};if opErr==nil {var key,lang string;key,lang,opErr=templateIdentity(template);if opErr==nil {expected:=key;if lang!="" {expected+="."+lang};if expected!=strings.TrimSuffix(name,".json") {opErr=errors.New("The prompt identifier or language does not match its filename")}}};if opErr==nil {templates=append(templates,template);continue};key:=strings.TrimSuffix(name,".json");lang:="";for _,candidate:=range []string{"zh-CN","en-US"} {if strings.HasSuffix(key,"."+candidate) {key=strings.TrimSuffix(key,"."+candidate);lang=candidate}};message:="The prompt file cannot be read";if data!=nil {if template==nil {message="The prompt content shape is invalid"} else {message=opErr.Error()}};diagnostic:=map[string]any{"key":key,"path":name,"error":message};if lang!="" {diagnostic["writingLanguage"]=lang};diagnostics=append(diagnostics,diagnostic)
 };result["templates"]=templates;result["diagnostics"]=diagnostics;return result,nil
}
func (s *Service) SavePrompt(template map[string]any) error {
 key,lang,err:=templateIdentity(template);if err!=nil{return err};if lang==""{lang="zh-CN"};name,err:=promptName(key,lang);if err!=nil{return err};copy:=map[string]any{};for k,v:=range template {copy[k]=v};copy["writingLanguage"]=lang;data,err:=json.MarshalIndent(copy,"","  ");if err!=nil{return err};if len(data)>MaxPromptBytes{return errors.New("Prompt exceeds native storage limit")};data=append(data,'\n')
 s.mu.Lock();defer s.mu.Unlock();if s.closed{return ErrClosed};dir,err:=openChildDir(s.root,"prompts",true,true);if err!=nil{return err};defer dir.close();if lang=="zh-CN" {legacy,_:=promptName(key,"");if info,e:=statAt(dir,legacy);e==nil&&!safeFile(info){return ErrUnsafe}else if e!=nil&&!errors.Is(e,fs.ErrNotExist){return e}};if err=atomicWrite(dir,name,data,false);err!=nil{return err};if lang=="zh-CN" {legacy,_:=promptName(key,"");return removeFile(dir,legacy)};return nil
}
func (s *Service) DeletePrompt(key,language string) error {
 if !promptLanguage(language,false){return errors.New("The writing language is invalid")};name,err:=promptName(key,language);if err!=nil{return err};s.mu.Lock();defer s.mu.Unlock();if s.closed{return ErrClosed};dir,err:=openChildDir(s.root,"prompts",false,true);if errors.Is(err,fs.ErrNotExist){return nil};if err!=nil{return err};defer dir.close();names:=[]string{name};if language=="zh-CN" {legacy,_:=promptName(key,"");names=append(names,legacy)};for _,name:=range names {if info,e:=statAt(dir,name);e==nil&&!safeFile(info){return ErrUnsafe}else if e!=nil&&!errors.Is(e,fs.ErrNotExist){return e}};for _,name:=range names {if err=removeFile(dir,name);err!=nil{return err}};return nil
}
func digest(data []byte) string {sum:=sha256.Sum256(data);return hex.EncodeToString(sum[:])}
func validText(data []byte) bool {return len(data)<=MaxSkillBytes&&utf8.Valid(data)&&!bytes.ContainsRune(data,0)}
func validBundlePath(p string) bool {if !textExtension(p){return false};for _,part:=range strings.Split(p,"/") {if !segmentPattern.MatchString(part)||part=="."||part==".."{return false}};return true}
func readBundle(root *ownedDir) Bundle {
 damaged:=Bundle{Status:"damaged"};data,err:=readAt(root,BundleManifest,MaxSkillBytes);if errors.Is(err,fs.ErrNotExist){return Bundle{Status:"none"}};if err!=nil{return damaged};var manifest struct {Version int `json:"version"`;Files []struct{Path string `json:"path"`;SHA256 string `json:"sha256"`} `json:"files"`};if json.Unmarshal(data,&manifest)!=nil||manifest.Version!=1||manifest.Files==nil||len(manifest.Files)>MaxBundledFiles{return damaged};seen:=map[string]bool{"skill.md":true};files:=[]BundledFile{};total:=0
 for _,entry:=range manifest.Files {lower:=strings.ToLower(entry.Path);if !validBundlePath(entry.Path)||seen[lower]||!rx(`^[a-f0-9]{64}$`,entry.SHA256){return damaged};seen[lower]=true;bytes,err:=readRelative(root,entry.Path,MaxSkillBytes);if err!=nil||!validText(bytes)||digest(bytes)!=entry.SHA256{return damaged};total+=len(bytes);if total>MaxSkillBytes{return damaged};files=append(files,BundledFile{entry.Path,string(bytes)})};return Bundle{Status:"intact",Files:files}
}
type UserSkill struct {Name string `json:"name"`;Content string `json:"content"`;BaseDir string `json:"baseDir"`;FilePath string `json:"filePath"`;Bundle Bundle `json:"bundle"`}
func (s *Service) ListUser()([]UserSkill,error) {
 s.mu.Lock();defer s.mu.Unlock();if s.closed{return nil,ErrClosed};skills:=[]UserSkill{};dir,err:=openChildDir(s.root,"skills",false,true);if errors.Is(err,fs.ErrNotExist){return skills,nil};if err!=nil{return nil,err};defer dir.close();items,err:=entries(dir);if err!=nil{return nil,err};sort.Slice(items,func(i,j int)bool{return items[i].Name()<items[j].Name()});for _,entry:=range items {if !entry.IsDir()||!ValidName(entry.Name()){continue};child,e:=openChildDir(dir,entry.Name(),false,true);if e!=nil{continue};raw,e:=readAt(child,"SKILL.md",MaxSkillBytes);if e==nil&&validText(raw) {base:=filepath.Join(s.rootPath,"skills",entry.Name());skills=append(skills,UserSkill{Name:entry.Name(),Content:string(raw),BaseDir:base,FilePath:filepath.Join(base,"SKILL.md"),Bundle:readBundle(child)})};child.close()};return skills,nil
}
func (s *Service) ReadUser(name string)(UserSkill,error) {if !ValidName(name){return UserSkill{},ErrUnsafe};list,err:=s.ListUser();if err!=nil{return UserSkill{},err};for _,skill:=range list {if skill.Name==name{return skill,nil}};return UserSkill{},fs.ErrNotExist}
func (s *Service) UninstallUser(name string) error {
 if !ValidName(name){return ErrUnsafe};s.mu.Lock();defer s.mu.Unlock();if s.closed{return ErrClosed};dir,err:=openChildDir(s.root,"skills",false,true);if errors.Is(err,fs.ErrNotExist){return nil};if err!=nil{return err};defer dir.close();if _,err=statAt(dir,name);errors.Is(err,fs.ErrNotExist){return nil}else if err!=nil{return err};return removeTree(dir,name)
}
