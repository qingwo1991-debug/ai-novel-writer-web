//go:build linux

package writerlock

import (
	"path/filepath"
	"strings"
)

// VerifyLegacy proves this exact live lock was acquired with the legacy gateway
// policy for the supplied canonical data root, not the unrelated private lock.
// It does not acquire a lock, transfer ownership or extend its lifetime. The
// runtime must retain this Lock until all admitted database handles are closed.
func (lock *Lock) VerifyLegacy(directory string) error {
	if lock == nil || directory == "/" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsAny(directory, "\x00\\") {
		return ErrUnsafe
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.file == nil || lock.policy != legacyLockPolicy || lock.anchor == nil || !lock.anchor.valid() || !sameEntryPolicy(lock.anchor, lock.file, lock.identity, lock.policy) {
		return ErrUnsafe
	}
	parts := lock.anchor.parts
	expected := strings.Split(strings.TrimPrefix(directory, "/"), "/")
	if len(parts) != len(expected)+1 {
		return ErrUnsafe
	}
	for index, name := range expected {
		if parts[index+1].name != name {
			return ErrUnsafe
		}
	}
	return nil
}
