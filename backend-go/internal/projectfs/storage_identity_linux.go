//go:build linux

package projectfs

import "syscall"

// StorageIdentity returns the device and inode of this project's pinned
// .ai-novel directory. It first verifies that the same directory is still
// visible through the pinned library capability; the returned numbers alone
// are observations, not write authority.
func (p *Project) StorageIdentity() (device, inode uint64, err error) {
	if p == nil || p.library == nil {
		return 0, 0, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return 0, 0, err
	}
	stat, ok := p.storageInfo.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, ErrChanged
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
