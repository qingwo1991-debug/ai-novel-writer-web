package projectfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
)

const snapshotStreamBuffer = 128 * 1024

// SnapshotProof retains only source identity, metadata and hashes, never author
// bytes, a destination handle, or additional filesystem authority. It borrows its
// Project: verification requires that Project and its Library to remain open.
// A zero-value or nil proof cannot be verified.
type SnapshotProof struct {
	source       *Project
	entries      map[string]snapshotEntry
	manifestInfo fs.FileInfo
	maxBytes     int64
}

type stagedDatabaseFile struct {
	name string
	file *os.File
	info fs.FileInfo
}

// StageDatabase streams the full DB/WAL/SHM/rollback-journal set into exclusively
// created snapshot.db{,-wal,-shm,-journal} files with mode 0600. maxBytes is a
// positive total-set budget, not subject to the legacy MaxSnapshotBytes ceiling.
// Database content uses one reusable 128 KiB buffer, independent of input size.
// The three passes hash a reference, hash while copying, and hash the source
// again. No content bytes or source descriptors are returned; no SQLite is opened.
//
// PRECONDITION: the caller MUST exclude source SQLite writers/checkpointers and
// hostile filesystem writers continuously through staging, private-copy SQLite
// verification, and proof.VerifyUnchanged. Sampling is NOT an atomic SQLite
// backup and cannot establish transaction consistency against uncooperative
// writers or swap-away-and-back attacks. Rollback journals are preserved, not
// replayed; recovery-required copies must be rejected by a read-only verifier.
//
// destination MUST be a caller-owned private 0700 directory, not any source
// project directory, with no concurrent mutation. Its path/ancestors must come
// from trusted caller configuration; Root.Name is deliberately not a security
// check. The caller owns destination's lifetime. All four destination names must
// be absent, even for absent source sidecars. Failure rolls back only files this
// invocation created, never preexisting files or observed replacements.
func (p *Project) StageDatabase(ctx context.Context, destination *os.Root, maxBytes int64) (proof *SnapshotProof, err error) {
	if ctx == nil || destination == nil {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		return nil, ErrTooLarge
	}
	if err := p.checkStageDestination(destination); err != nil {
		return nil, err
	}
	_, manifestInfo, _, err := p.readManifest()
	if err != nil {
		return nil, err
	}
	var created []stagedDatabaseFile
	defer func() {
		for _, owned := range created {
			closeErr := owned.file.Close()
			if errors.Is(closeErr, os.ErrClosed) {
				closeErr = nil
			}
			err = errors.Join(err, closeErr)
		}
		if err != nil {
			proof = nil
			err = errors.Join(err, rollbackStagedDatabase(destination, created))
		}
	}()
	buffer := make([]byte, snapshotStreamBuffer)
	before, err := p.streamSnapshotPass(ctx, maxBytes, buffer, nil, nil)
	if err != nil {
		return nil, err
	}
	copied, err := p.streamSnapshotPass(ctx, maxBytes, buffer, destination, &created)
	if err != nil {
		return nil, err
	}
	if !sameSnapshot(before, copied) {
		return nil, ErrChanged
	}
	// Close all writes before reporting success or letting SQLite see the copy.
	for _, owned := range created {
		if err := owned.file.Close(); err != nil {
			return nil, err
		}
	}
	after, err := p.streamSnapshotPass(ctx, maxBytes, buffer, nil, nil)
	if err != nil {
		return nil, err
	}
	if !sameSnapshot(copied, after) {
		return nil, ErrChanged
	}
	proof = &SnapshotProof{source: p, entries: before, manifestInfo: manifestInfo, maxBytes: maxBytes}
	if err := proof.checkIdentityLocked(ctx); err != nil {
		return nil, err
	}
	return proof, nil
}

func (p *Project) checkStageDestination(destination *os.Root) error {
	info, err := destination.Stat(".")
	if err != nil {
		return err
	}
	if !directory(info) || info.Mode().Perm() != 0700 || os.SameFile(info, p.storageInfo) || os.SameFile(info, p.info) || os.SameFile(info, p.library.info) {
		return ErrUnsafeDirectory
	}
	for _, suffix := range databaseSuffixes {
		if _, err := destination.Lstat("snapshot.db" + suffix); err == nil {
			return fs.ErrExist
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func rollbackStagedDatabase(destination *os.Root, created []stagedDatabaseFile) error {
	var result error
	for _, owned := range created {
		named, err := destination.Lstat(owned.name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		// No destination writer is allowed by the API contract, but do not delete
		// someone else's inode even if that precondition was violated.
		if owned.info == nil || !os.SameFile(owned.info, named) {
			result = errors.Join(result, ErrChanged)
			continue
		}
		result = errors.Join(result, destination.Remove(owned.name))
	}
	return result
}

func (p *Project) streamSnapshotPass(ctx context.Context, limit int64, buffer []byte, destination *os.Root, created *[]stagedDatabaseFile) (map[string]snapshotEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	members, err := p.snapshotMembership()
	if err != nil {
		return nil, err
	}
	entries := make(map[string]snapshotEntry, len(members))
	remaining := limit
	for _, suffix := range databaseSuffixes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		before, exists := members[suffix]
		if !exists {
			continue
		}
		var writer io.Writer
		if destination != nil {
			name := "snapshot.db" + suffix
			file, err := destination.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return nil, err
			}
			*created = append(*created, stagedDatabaseFile{name: name, file: file})
			info, err := file.Stat()
			(*created)[len(*created)-1].info = info
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() || !singleLink(info) {
				return nil, ErrUnsafeFile
			}
			if err := file.Chmod(0600); err != nil {
				return nil, err
			}
			writer = file
		}
		entry, size, err := p.streamSnapshotFile(ctx, DatabaseName+suffix, remaining, buffer, writer)
		if err != nil {
			return nil, err
		}
		if !sameVersion(before, entry.info) {
			return nil, ErrChanged
		}
		remaining -= size
		entries[suffix] = entry
	}
	after, err := p.snapshotMembership()
	if err != nil {
		return nil, err
	}
	if len(after) != len(entries) {
		return nil, ErrChanged
	}
	for suffix, entry := range entries {
		info, exists := after[suffix]
		if !exists || !sameVersion(entry.info, info) {
			return nil, ErrChanged
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (p *Project) streamSnapshotFile(ctx context.Context, name string, limit int64, buffer []byte, writer io.Writer) (snapshotEntry, int64, error) {
	var empty snapshotEntry
	file, before, err := openRegular(p.storage, name)
	if err != nil {
		return empty, 0, err
	}
	defer file.Close()
	if limit < 0 || before.Size() < 0 || before.Size() > limit {
		return empty, 0, ErrTooLarge
	}
	hash := sha256.New()
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return empty, 0, err
		}
		chunk := buffer
		// Read at most the remaining budget plus one sentinel byte. Compare
		// before adding one, so even a MaxInt64 caller budget cannot overflow.
		readLimit := limit
		if before.Size() < readLimit {
			readLimit = before.Size()
		}
		if remaining := readLimit - size; remaining < int64(len(chunk)) {
			chunk = chunk[:int(remaining)+1]
		}
		n, readErr := file.Read(chunk)
		if err := ctx.Err(); err != nil {
			return empty, 0, err
		}
		if int64(n) > limit-size {
			return empty, 0, ErrTooLarge
		}
		if int64(n) > before.Size()-size {
			return empty, 0, ErrChanged
		}
		if n > 0 {
			_, _ = hash.Write(chunk[:n])
			if writer != nil {
				written, err := writer.Write(chunk[:n])
				if err != nil {
					return empty, 0, err
				}
				if written != n {
					return empty, 0, io.ErrShortWrite
				}
			}
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return empty, 0, readErr
		}
		if n == 0 {
			return empty, 0, io.ErrNoProgress
		}
	}
	after, statErr := file.Stat()
	named, nameErr := p.storage.Lstat(name)
	if statErr != nil || nameErr != nil || !sameVersion(before, after) || !sameVersion(after, named) || size != after.Size() {
		return empty, 0, ErrChanged
	}
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return snapshotEntry{info: after, hash: digest}, size, nil
}

func (proof *SnapshotProof) checkIdentityLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := proof.source.checkIdentityLocked(); err != nil {
		return err
	}
	_, info, _, err := proof.source.readManifest()
	if err != nil {
		return err
	}
	if !sameVersion(proof.manifestInfo, info) {
		return ErrManifestChanged
	}
	return ctx.Err()
}

// VerifyUnchanged rehashes and revalidates the captured source set, membership,
// inode/stat identities, manifest and pinned directories after private-copy
// admission. It never consults the destination (SQLite may have changed it).
// Source writer exclusion must still be held; this is not an atomic backup proof.
func (proof *SnapshotProof) VerifyUnchanged(ctx context.Context) error {
	if proof == nil || proof.source == nil || len(proof.entries) == 0 {
		return ErrClosed
	}
	if ctx == nil {
		return ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := proof.source
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := proof.checkIdentityLocked(ctx); err != nil {
		return err
	}
	entries, err := p.streamSnapshotPass(ctx, proof.maxBytes, make([]byte, snapshotStreamBuffer), nil, nil)
	if err != nil {
		return err
	}
	if !sameSnapshot(proof.entries, entries) {
		return ErrChanged
	}
	return proof.checkIdentityLocked(ctx)
}
