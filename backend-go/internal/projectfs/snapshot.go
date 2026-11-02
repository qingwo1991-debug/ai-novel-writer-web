package projectfs

import (
	"crypto/sha256"
	"errors"
	"io/fs"
)

// DatabaseSnapshot owns independent bytes, never a source database path or
// descriptor. Files uses only these exact suffix keys: "", "-wal", "-shm",
// "-journal". "" is always present; absent sidecars must remain absent when
// staged. The consumer must create a private 0700 staging directory, write 0600
// owned files, and run SQLite admission against that copy, never the originals.
type DatabaseSnapshot struct {
	Files map[string][]byte
}

var databaseSuffixes = [...]string{"", "-wal", "-shm", "-journal"}

type snapshotEntry struct {
	info fs.FileInfo
	hash [32]byte
}

func (p *Project) snapshotMembership() (map[string]fs.FileInfo, error) {
	members := make(map[string]fs.FileInfo)
	for _, suffix := range databaseSuffixes {
		info, err := p.storage.Lstat(DatabaseName + suffix)
		if errors.Is(err, fs.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || !singleLink(info) {
			return nil, ErrUnsafeFile
		}
		members[suffix] = info
	}
	return members, nil
}

func (p *Project) snapshotPass(limit int64, keep bool) (map[string]snapshotEntry, map[string][]byte, error) {
	members, err := p.snapshotMembership()
	if err != nil {
		return nil, nil, err
	}
	entries := make(map[string]snapshotEntry)
	var files map[string][]byte
	if keep {
		files = make(map[string][]byte)
	}
	remaining := limit
	for _, suffix := range databaseSuffixes {
		before, exists := members[suffix]
		if !exists {
			continue
		}
		data, info, err := readBounded(p.storage, DatabaseName+suffix, remaining)
		if err != nil {
			return nil, nil, err
		}
		if !sameVersion(before, info) {
			return nil, nil, ErrChanged
		}
		remaining -= int64(len(data))
		entries[suffix] = snapshotEntry{info: info, hash: sha256.Sum256(data)}
		if keep {
			files[suffix] = data
		}
	}
	after, err := p.snapshotMembership()
	if err != nil {
		return nil, nil, err
	}
	if len(after) != len(entries) {
		return nil, nil, ErrChanged
	}
	for suffix, entry := range entries {
		info, exists := after[suffix]
		if !exists || !sameVersion(entry.info, info) {
			return nil, nil, ErrChanged
		}
	}
	return entries, files, nil
}

func sameSnapshot(a, b map[string]snapshotEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for suffix, entry := range a {
		other, exists := b[suffix]
		if !exists || entry.hash != other.hash || !sameVersion(entry.info, other.info) {
			return false
		}
	}
	return true
}

// SnapshotDatabase performs three bounded descriptor-read passes over the full
// DB/WAL/SHM/rollback-journal set: reference, owned copy, and post-copy validation.
// Every pass checks membership, inode identity, nlink, size, mtime, ctime and
// SHA-256; manifest and directory identity are revalidated around the operation.
//
// PRECONDITION: the caller must exclude source SQLite writers/checkpointers (and
// hostile filesystem writers) for the complete snapshot plus verification window.
// Sampling is NOT an atomic SQLite backup and cannot prove cross-file transaction
// consistency against an uncooperative writer or a swap-away-and-back attacker.
// Rollback journals are preserved, not replayed here; recovery-required copies
// must be rejected by the consumer's read-only verifier. This method never opens
// original files with SQLite or writes any project data.
func (p *Project) SnapshotDatabase(maxBytes int64) (DatabaseSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	var empty DatabaseSnapshot
	if err := p.checkIdentityLocked(); err != nil {
		return empty, err
	}
	if maxBytes <= 0 || maxBytes > MaxSnapshotBytes {
		return empty, ErrTooLarge
	}
	before, _, err := p.snapshotPass(maxBytes, false)
	if err != nil {
		return empty, err
	}
	copied, files, err := p.snapshotPass(maxBytes, true)
	if err != nil {
		return empty, err
	}
	if !sameSnapshot(before, copied) {
		return empty, ErrChanged
	}
	after, _, err := p.snapshotPass(maxBytes, false)
	if err != nil {
		return empty, err
	}
	if !sameSnapshot(copied, after) {
		return empty, ErrChanged
	}
	if err := p.checkIdentityLocked(); err != nil {
		return empty, err
	}
	return DatabaseSnapshot{Files: files}, nil
}
