//go:build linux && amd64

package globalmigration

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
)

var stageTempSuffix = regexp.MustCompile(`\.[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}\.tmp$`)

var stageObjects = map[string]bool{
	"config.json": true, "models.json": true, "recent-projects.json": true,
	"mcp_config.json": true, "prompts": true, "skills": true, "skins": true,
}

type stageDigest struct {
	size int64
	sum  [32]byte
}

// VerifyPrepared advances only the journal-owned staging directory from
// PREPARED to VERIFIED. It has no installation, receipt, or startup authority.
// The caller holds global exclusion; all same-UID processes must cooperate.
func VerifyPrepared(ctx context.Context, roots globaladmission.Roots, exclusive bool) (Prepared, error) {
	var zero Prepared
	if ctx == nil || !exclusive {
		return zero, fail("GLOBAL_EXCLUSIVE_ACCESS_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	canonicalInfo, parentInfo, err := disposableRoots(roots)
	if err != nil {
		return zero, err
	}
	legacyInfo, legacyErr := os.Lstat(roots.LegacySource)
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return zero, fail("GLOBAL_ROOT_UNSAFE")
	}
	source, err := globaladmission.PreflightSource(ctx, roots)
	if err != nil {
		return zero, err
	}
	pin, err := pinCanonical(roots.CanonicalTarget, canonicalInfo, parentInfo)
	if err != nil {
		return zero, err
	}
	defer pin.close()
	if err := pin.verify(); err != nil {
		return zero, err
	}
	controlFD, err := stageOpenDir(pin.rootFD, ".migration")
	if err != nil {
		return zero, err
	}
	defer syscall.Close(controlFD)
	genFD, err := stageOpenDir(pin.rootFD, "generations")
	if err != nil {
		return zero, err
	}
	defer syscall.Close(genFD)
	if exists, err := entryExistsAt(controlFD, "receipt.json"); err != nil {
		return zero, err
	} else if exists {
		return zero, fail("GLOBAL_RECEIPT_PRESENT")
	}
	j, err := readJournalAnyAt(controlFD)
	if err != nil {
		return zero, err
	}
	if j.Source != source {
		return zero, fail("GLOBAL_SOURCE_CHANGED")
	}
	stagingName := j.Generation + ".staging"
	installed, err := entryExistsAt(genFD, j.Generation)
	if err != nil {
		return zero, err
	}
	if installed {
		return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
	}
	if err := stageCheckGenerations(genFD, stagingName); err != nil {
		return zero, err
	}
	pending, err := stagePendingJournal(controlFD, j)
	if err != nil {
		return zero, err
	}
	if j.Phase == "verified" && pending != "" {
		return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
	}
	sourceFD := pin.rootFD
	if j.Source == "legacy" {
		if legacyInfo == nil {
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
		sourceFD, err = syscall.Openat(pin.parentFD, filepath.Base(roots.LegacySource), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
		defer syscall.Close(sourceFD)
		if !samePinned(sourceFD, legacyInfo) {
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	if j.Source == "legacy" {
		if err := stageVerifyDirectory(pin.parentFD, filepath.Base(roots.LegacySource), sourceFD); err != nil {
			return zero, err
		}
	}
	before, err := stageFiles(ctx, sourceFD, false)
	if err != nil {
		return zero, err
	}
	stagingFD, err := stageOpenDir(genFD, stagingName)
	if errors.Is(err, syscall.ENOENT) && j.Phase == "prepared" {
		if err := pin.verify(); err != nil {
			return zero, err
		}
		if err := syscall.Mkdirat(genFD, stagingName, 0700); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		if err := syscall.Fsync(genFD); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		stagingFD, err = stageOpenDir(genFD, stagingName)
	}
	if err != nil {
		return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
	}
	defer syscall.Close(stagingFD)
	if err := stageVerifyDirectory(pin.rootFD, ".migration", controlFD); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(pin.rootFD, "generations", genFD); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(genFD, stagingName, stagingFD); err != nil {
		return zero, err
	}
	if j.Phase == "prepared" {
		if err := stageResumeAndCopy(ctx, sourceFD, stagingFD, before, pin); err != nil {
			return zero, err
		}
	}
	if err := stageParity(ctx, sourceFD, stagingFD, before, j.Phase); err != nil {
		return zero, err
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(genFD, stagingName, stagingFD); err != nil {
		return zero, err
	}
	// The validator opens descendants through this pinned directory FD, rather
	// than resolving the public staging path a second time.
	if err := globaladmission.ValidateGeneration("/proc/self/fd/" + strconv.Itoa(stagingFD)); err != nil {
		return zero, err
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(pin.rootFD, ".migration", controlFD); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(pin.rootFD, "generations", genFD); err != nil {
		return zero, err
	}
	if err := stageVerifyDirectory(genFD, stagingName, stagingFD); err != nil {
		return zero, err
	}
	if j.Source == "legacy" {
		if err := stageVerifyDirectory(pin.parentFD, filepath.Base(roots.LegacySource), sourceFD); err != nil {
			return zero, err
		}
	}
	if err := stageParity(ctx, sourceFD, stagingFD, before, j.Phase); err != nil {
		return zero, err
	}
	if j.Phase == "prepared" {
		if err := stageSyncDirs(stagingFD); err != nil {
			return zero, err
		}
		if err := syscall.Fsync(genFD); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
	}
	if j.Phase == "prepared" {
		if err := stageReplaceJournal(controlFD, j, pending, pin); err != nil {
			return zero, err
		}
		j.Phase = "verified"
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	return Prepared{Generation: j.Generation, Source: j.Source, Phase: j.Phase}, nil
}

func stageOpenDir(parentFD int, name string) (int, error) {
	return syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

func stageVerifyDirectory(parentFD int, name string, pinnedFD int) error {
	current, err := stageOpenDir(parentFD, name)
	if err != nil {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	defer syscall.Close(current)
	var actual, expected syscall.Stat_t
	if syscall.Fstat(current, &actual) != nil || syscall.Fstat(pinnedFD, &expected) != nil ||
		actual.Dev != expected.Dev || actual.Ino != expected.Ino {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	return nil
}

func stageNames(fd int) ([]string, error) {
	fresh, err := stageOpenDir(fd, ".")
	if err != nil {
		return nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	file := os.NewFile(uintptr(fresh), "directory")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func stageCheckGenerations(fd int, staging string) error {
	names, err := stageNames(fd)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name != staging {
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
	}
	return nil
}

func stagePendingJournal(fd int, current journal) (string, error) {
	names, err := stageNames(fd)
	if err != nil {
		return "", err
	}
	pending := ""
	for _, name := range names {
		if name == "journal.json" {
			continue
		}
		if !strings.HasPrefix(name, "journal.json.") || !stageTempSuffix.MatchString(name) || pending != "" {
			return "", fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		value, err := readJournalNamedAt(fd, name)
		if err != nil || value.Version != current.Version || value.Generation != current.Generation ||
			value.Source != current.Source || value.Phase != "verified" {
			return "", fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		pending = name
	}
	return pending, nil
}

func stageFiles(ctx context.Context, rootFD int, allowTemp bool) (map[string]stageDigest, error) {
	result := make(map[string]stageDigest)
	var walk func(int, string, bool) error
	walk = func(dirFD int, prefix string, top bool) error {
		names, err := stageNames(dirFD)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if name == "." || name == ".." || strings.ContainsRune(name, '/') {
				return fail("GLOBAL_ASSET_TYPE")
			}
			if top && !stageObjects[name] {
				base := stageTempSuffix.ReplaceAllString(name, "")
				if allowTemp && base != name && stageObjects[base] && strings.HasSuffix(base, ".json") {
					// A crash may leave an exclusive temp beside a top-level JSON object.
				} else if allowTemp {
					return fail("GLOBAL_STAGING_UNKNOWN_OBJECT")
				} else {
					continue
				}
			}
			relative := name
			if prefix != "" {
				relative = prefix + "/" + name
			}
			probe, err := syscall.Openat(dirFD, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				return fail("GLOBAL_ASSET_TYPE")
			}
			var stat syscall.Stat_t
			err = syscall.Fstat(probe, &stat)
			syscall.Close(probe)
			if err != nil {
				return fail("GLOBAL_ASSET_TYPE")
			}
			if stat.Mode&syscall.S_IFMT == syscall.S_IFLNK {
				return fail("GLOBAL_ASSET_LINK")
			}
			if stat.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				if top && strings.HasSuffix(name, ".json") {
					return fail("GLOBAL_ASSET_TYPE")
				}
				next, err := stageOpenDir(dirFD, name)
				if err != nil {
					return fail("GLOBAL_ASSET_LINK")
				}
				err = walk(next, relative, false)
				syscall.Close(next)
				if err != nil {
					return err
				}
				continue
			}
			if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
				return fail("GLOBAL_ASSET_TYPE")
			}
			if top && !strings.HasSuffix(name, ".json") && !(allowTemp && stageTempSuffix.MatchString(name)) {
				return fail("GLOBAL_ASSET_TYPE")
			}
			if stat.Nlink != 1 {
				return fail("GLOBAL_ASSET_HARDLINK")
			}
			if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 {
				return fail("GLOBAL_ASSET_TYPE")
			}
			digest, err := stageHashAt(dirFD, name)
			if err != nil {
				return err
			}
			result[relative] = digest
		}
		return nil
	}
	if err := walk(rootFD, "", true); err != nil {
		return nil, err
	}
	return result, nil
}

func stageHashAt(parentFD int, name string) (stageDigest, error) {
	var zero stageDigest
	fd, err := syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return zero, fail("GLOBAL_ASSET_LINK")
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return zero, fail("GLOBAL_ASSET_TYPE")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return zero, fail("GLOBAL_ASSET_HARDLINK")
	}
	h := sha256.New()
	size, err := io.Copy(h, file)
	if err != nil || size != info.Size() {
		return zero, fail("GLOBAL_SOURCE_CHANGED")
	}
	copy(zero.sum[:], h.Sum(nil))
	zero.size = size
	return zero, nil
}

func stageOpenParent(rootFD int, relative string, create bool) (int, string, error) {
	parts := strings.Split(relative, "/")
	if len(parts) == 0 || path.Clean(relative) != relative || strings.HasPrefix(relative, "/") {
		return -1, "", fail("GLOBAL_ASSET_TYPE")
	}
	current, err := stageOpenDir(rootFD, ".")
	if err != nil {
		return -1, "", fail("GLOBAL_ROOT_UNSAFE")
	}
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			syscall.Close(current)
			return -1, "", fail("GLOBAL_ASSET_TYPE")
		}
		next, err := stageOpenDir(current, part)
		if errors.Is(err, syscall.ENOENT) && create {
			if err = syscall.Mkdirat(current, part, 0700); err == nil {
				err = syscall.Fsync(current)
			}
			if err == nil {
				next, err = stageOpenDir(current, part)
			}
		}
		syscall.Close(current)
		if err != nil {
			return -1, "", fail("GLOBAL_ASSET_LINK")
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func stageResumeAndCopy(ctx context.Context, sourceFD, stagingFD int, source map[string]stageDigest, pin *pinnedCanonical) error {
	staged, err := stageFiles(ctx, stagingFD, true)
	if err != nil {
		return err
	}
	temps := make(map[string]string)
	for name, digest := range staged {
		if expected, ok := source[name]; ok {
			if digest != expected {
				return fail("GLOBAL_STAGING_MISMATCH")
			}
			continue
		}
		base := stageTempSuffix.ReplaceAllString(name, "")
		expected, ok := source[base]
		if !ok || base == name || digest != expected || temps[base] != "" {
			return fail("GLOBAL_STAGING_MISMATCH")
		}
		temps[base] = name
	}
	names := make([]string, 0, len(source))
	for name := range source {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := staged[name]; exists {
			if temps[name] != "" {
				return fail("GLOBAL_STAGING_MISMATCH")
			}
			if err := stageFsyncRetained(stagingFD, name); err != nil {
				return err
			}
			continue
		}
		if err := pin.verify(); err != nil {
			return err
		}
		if temp := temps[name]; temp != "" {
			parentFD, leaf, err := stageOpenParent(stagingFD, name, false)
			if err != nil {
				return err
			}
			tempFD, openErr := syscall.Openat(parentFD, path.Base(temp), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if openErr != nil {
				syscall.Close(parentFD)
				return fail("GLOBAL_STAGING_MISMATCH")
			}
			var stat syscall.Stat_t
			if syscall.Fstat(tempFD, &stat) != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
				stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || syscall.Fsync(tempFD) != nil {
				syscall.Close(tempFD)
				syscall.Close(parentFD)
				return fail("GLOBAL_STAGING_MISMATCH")
			}
			syscall.Close(tempFD)
			err = stageRenameNoReplace(parentFD, path.Base(temp), parentFD, leaf)
			if err == nil {
				err = syscall.Fsync(parentFD)
			}
			syscall.Close(parentFD)
			if err != nil {
				return fail("GLOBAL_MIGRATION_IO_FAILED")
			}
		} else if err := stageCopyOne(sourceFD, stagingFD, name, source[name]); err != nil {
			return err
		}
		if err := pin.verify(); err != nil {
			return err
		}
	}
	return nil
}

func stageFsyncRetained(rootFD int, relative string) error {
	parentFD, leaf, err := stageOpenParent(rootFD, relative, false)
	if err != nil {
		return err
	}
	defer syscall.Close(parentFD)
	fd, err := syscall.Openat(parentFD, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail("GLOBAL_STAGING_MISMATCH")
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if syscall.Fstat(fd, &stat) != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return fail("GLOBAL_STAGING_MISMATCH")
	}
	if syscall.Fsync(fd) != nil || syscall.Fsync(parentFD) != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}

func stageCopyOne(sourceFD, stagingFD int, name string, expected stageDigest) error {
	sourceParent, sourceLeaf, err := stageOpenParent(sourceFD, name, false)
	if err != nil {
		return err
	}
	defer syscall.Close(sourceParent)
	sourceFileFD, err := syscall.Openat(sourceParent, sourceLeaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	sourceFile := os.NewFile(uintptr(sourceFileFD), sourceLeaf)
	defer sourceFile.Close()
	info, err := sourceFile.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	targetParent, targetLeaf, err := stageOpenParent(stagingFD, name, true)
	if err != nil {
		return err
	}
	defer syscall.Close(targetParent)
	id, err := newUUID()
	if err != nil {
		return err
	}
	temporary := targetLeaf + "." + id + ".tmp"
	fd, err := syscall.Openat(targetParent, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	targetFile := os.NewFile(uintptr(fd), temporary)
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(targetFile, h), sourceFile)
	if err != nil || size != expected.size {
		targetFile.Close()
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	var actual [32]byte
	copy(actual[:], h.Sum(nil))
	if actual != expected.sum {
		targetFile.Close()
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	if err := targetFile.Sync(); err != nil {
		targetFile.Close()
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := targetFile.Close(); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := stageRenameNoReplace(targetParent, temporary, targetParent, targetLeaf); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Fsync(targetParent); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}

func stageParity(ctx context.Context, sourceFD, stagingFD int, before map[string]stageDigest, phase string) error {
	source, err := stageFiles(ctx, sourceFD, false)
	if err != nil {
		return err
	}
	if !stageSameFiles(before, source) {
		return fail("GLOBAL_SOURCE_CHANGED")
	}
	staging, err := stageFiles(ctx, stagingFD, true)
	if err != nil {
		return err
	}
	if !stageSameFiles(source, staging) {
		if phase == "verified" {
			return fail("GLOBAL_VERIFIED_SOURCE_CHANGED")
		}
		return fail("GLOBAL_STAGING_MISMATCH")
	}
	return nil
}

func stageSameFiles(a, b map[string]stageDigest) bool {
	if len(a) != len(b) {
		return false
	}
	for name, digest := range a {
		if b[name] != digest {
			return false
		}
	}
	return true
}

func stageSyncDirs(fd int) error {
	names, err := stageNames(fd)
	if err != nil {
		return err
	}
	for _, name := range names {
		child, err := stageOpenDir(fd, name)
		if errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return fail("GLOBAL_ROOT_UNSAFE")
		}
		err = stageSyncDirs(child)
		syscall.Close(child)
		if err != nil {
			return err
		}
	}
	if err := syscall.Fsync(fd); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}

func stageRenameNoReplace(oldFD int, oldName string, newFD int, newName string) error {
	oldPtr, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPtr, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(renameat2Trap, uintptr(oldFD), uintptr(unsafe.Pointer(oldPtr)), uintptr(newFD), uintptr(unsafe.Pointer(newPtr)), 1, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func stageReplaceJournal(controlFD int, prepared journal, pending string, pin *pinnedCanonical) error {
	verified := prepared
	verified.Phase = "verified"
	if pending != "" {
		if err := stageFsyncRetained(controlFD, pending); err != nil {
			return err
		}
	} else {
		data, err := json.Marshal(verified)
		if err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		id, err := newUUID()
		if err != nil {
			return err
		}
		pending = "journal.json." + id + ".tmp"
		fd, err := syscall.Openat(controlFD, pending, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		file := os.NewFile(uintptr(fd), pending)
		if _, err = file.Write(data); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		if err := syscall.Fsync(controlFD); err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
	}
	if err := pin.verify(); err != nil {
		return err
	}
	current, err := readJournalAnyAt(controlFD)
	if err != nil || current != prepared {
		return fail("GLOBAL_JOURNAL_INVALID")
	}
	if err := syscall.Renameat(controlFD, pending, controlFD, "journal.json"); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Fsync(controlFD); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}
