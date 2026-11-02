//go:build !linux
package knowledgecopy
import("io/fs";"os")
func supported()bool{return false}
func regular(fs.FileInfo)bool{return false}
func privateDirectory(fs.FileInfo)bool{return false}
func sameEntry(fs.FileInfo,fs.FileInfo)bool{return false}
func openRegular(*os.Root,string)(*os.File,error){return nil,ErrUnsupported}
func openDirectory(*os.Root)(*os.File,error){return nil,ErrUnsupported}
