//go:build linux
package projectaccess
import("os";"path/filepath";"syscall")
// Guard administrative scratch configuration even when a privileged process can
// traverse a foreign-owned0700 directory. No chmod/chown or repair is attempted.
func safeScratch(path string)bool{for current:=path;;current=filepath.Dir(current){info,err:=os.Lstat(current);if err!=nil||!info.IsDir()||info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid)!=0{return false};stat,ok:=info.Sys().(*syscall.Stat_t);if !ok{return false};if current==path{if stat.Uid!=uint32(os.Geteuid())||info.Mode().Perm()!=0700{return false}}else if (stat.Uid!=0&&stat.Uid!=uint32(os.Geteuid()))||(info.Mode().Perm()&0022!=0&&!(stat.Uid==0&&info.Mode()&os.ModeSticky!=0)){return false};if current==string(filepath.Separator){return true}}}
