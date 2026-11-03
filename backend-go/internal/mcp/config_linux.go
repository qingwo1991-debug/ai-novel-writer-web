//go:build linux

package mcp

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// readConfigFile admits the administrator's credential-bearing configuration
// as a single regular private inode. A path swap or symlink must never turn a
// reload into a read of another file. Missing is reported separately so the
// existing host-visible "missing" state remains meaningful.
func readConfigFile(path string) ([]byte, error) {
	invalid := errors.New("MCP_CONFIG_INVALID")
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !privateConfigFile(before) || before.Size() > MaxConfigBytes {
		return nil, invalid
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, invalid
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !privateConfigFile(opened) {
		return nil, invalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil || len(raw) > MaxConfigBytes {
		return nil, invalid
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || !privateConfigFile(after) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || int64(len(raw)) != after.Size() {
		return nil, invalid
	}
	return raw, nil
}

func privateConfigFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}
