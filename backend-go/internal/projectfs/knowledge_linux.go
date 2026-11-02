//go:build linux

package projectfs

import (
	"io/fs"
	"os"
	"syscall"
)

func knowledgeOwner(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func knowledgePrivate(info fs.FileInfo) bool {
	return directory(info) && info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0700 && knowledgeOwner(info)
}
func knowledgePrivateFile(info fs.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0600 && singleLink(info) && knowledgeOwner(info)
}
