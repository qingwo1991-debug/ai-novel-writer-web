//go:build linux

package projectfs

import (
	"io/fs"
	"os"
	"syscall"
)

func platformSupported() bool { return true }

// O_NOFOLLOW applies to the final component; all parent components have
// separately pinned Roots. O_NONBLOCK prevents a raced FIFO from hanging.
func readFlags() int { return os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK }

func singleLink(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func sameChangeTime(a, b fs.FileInfo) bool {
	x, xOK := a.Sys().(*syscall.Stat_t)
	y, yOK := b.Sys().(*syscall.Stat_t)
	return xOK && yOK && x.Ctim == y.Ctim
}
