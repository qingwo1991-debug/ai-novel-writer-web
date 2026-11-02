package projectfs

import (
 "io/fs"
 "os"
)
// Identity is an opaque comparison token for the physical directories and
// manifest admitted by OpenProject. It grants no file handles or path authority;
// it does not claim the project is still present/unchanged. Validate the live
// capability separately before using its token to detect same-UUID replacements.
type Identity struct{root,storage,manifest fs.FileInfo}
func(p *Project)Identity()Identity{if p==nil{return Identity{}};return Identity{p.info,p.storageInfo,p.manifestInfo}}
func(a Identity)Same(b Identity)bool{
 return a.root!=nil&&b.root!=nil&&a.storage!=nil&&b.storage!=nil&&a.manifest!=nil&&b.manifest!=nil&&os.SameFile(a.root,b.root)&&os.SameFile(a.storage,b.storage)&&os.SameFile(a.manifest,b.manifest)
}
