//go:build linux

package mcphost

import (
	"io"
	"os"
	"syscall"
)

// readConfigFile admits the author's credential-bearing MCP configuration as one
// regular private file with no symlink, no other owner, no group or world access
// and no second link, so a path swap between the check and the read cannot turn
// a load into a read of another file. It Lstats first only to keep "the file is not
// there" distinguishable from "the file may not be read"; the descriptor that is
// actually read is opened with O_NOFOLLOW and proven to be the same inode before,
// during and after the read.
func readConfigFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		// A missing file is the honest unconfigured state and must stay
		// distinguishable from an unreadable one.
		return nil, err
	}
	if !privateConfigFile(before) || before.Size() > MaxConfigBytes {
		return nil, ErrConfigUnreadable
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrConfigUnreadable
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !privateConfigFile(opened) {
		return nil, ErrConfigUnreadable
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil || len(raw) > MaxConfigBytes {
		return nil, ErrConfigUnreadable
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || !privateConfigFile(after) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) ||
		int64(len(raw)) != after.Size() {
		return nil, ErrConfigUnreadable
	}
	return raw, nil
}

// privateConfigFile is the admission every byte of that configuration has to
// pass: one regular file, owned by this process, readable and writable only by
// its owner, with no second link that could publish it elsewhere.
func privateConfigFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}
