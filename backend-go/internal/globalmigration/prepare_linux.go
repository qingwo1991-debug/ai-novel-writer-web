//go:build linux && amd64

// Package globalmigration contains incomplete, disposable-only stages of the
// original global-data migration. A PREPARED journal is not a ready generation.
package globalmigration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
)

type Prepared struct {
	Generation string
	Source     string
	Phase      string
}

type Failure struct{ Code string }

func (f *Failure) Error() string { return f.Code }
func fail(code string) error     { return &Failure{Code: code} }
func Code(err error) string {
	var own *Failure
	if errors.As(err, &own) {
		return own.Code
	}
	return globaladmission.Code(err)
}

type journal struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
	Phase      string `json:"phase"`
	Source     string `json:"source"`
}

const (
	renameat2Trap = 316      // linux/amd64; this incomplete writer is not built on other architectures.
	oPath         = 0x200000 // Linux O_PATH permits an FD-relative nofollow existence probe.
)

type pinnedCanonical struct {
	parentFD   int
	rootFD     int
	parentInfo os.FileInfo
	rootInfo   os.FileInfo
	chain      []pinnedComponent
}

type pinnedComponent struct {
	name string
	dev  uint64
	ino  uint64
}

func (p *pinnedCanonical) close() {
	if p == nil {
		return
	}
	_ = syscall.Close(p.rootFD)
	_ = syscall.Close(p.parentFD)
}

func samePinned(fd int, expected os.FileInfo) bool {
	var stat syscall.Stat_t
	if syscall.Fstat(fd, &stat) != nil {
		return false
	}
	want, ok := expected.Sys().(*syscall.Stat_t)
	return ok && stat.Dev == want.Dev && stat.Ino == want.Ino && stat.Mode&syscall.S_IFMT == syscall.S_IFDIR
}

// Open every ancestor from / without following links. Checking only the last
// component would permit a swapped intermediate directory to redirect writes.
func pinCanonical(path string, rootInfo, parentInfo os.FileInfo) (*pinnedCanonical, error) {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	parentFD := -1
	chain := make([]pinnedComponent, 0)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			syscall.Close(fd)
			if parentFD >= 0 {
				syscall.Close(parentFD)
			}
			return nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if openErr != nil {
			if parentFD >= 0 {
				syscall.Close(parentFD)
			}
			return nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		fd = next
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil {
			syscall.Close(fd)
			if parentFD >= 0 {
				syscall.Close(parentFD)
			}
			return nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		chain = append(chain, pinnedComponent{name: part, dev: stat.Dev, ino: stat.Ino})
		if i == len(parts)-2 {
			parentFD, err = syscall.Dup(fd)
			if err != nil {
				syscall.Close(fd)
				return nil, fail("GLOBAL_ROOT_UNSAFE")
			}
		}
	}
	if parentFD < 0 || !samePinned(parentFD, parentInfo) || !samePinned(fd, rootInfo) {
		syscall.Close(fd)
		if parentFD >= 0 {
			syscall.Close(parentFD)
		}
		return nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	return &pinnedCanonical{parentFD: parentFD, rootFD: fd, parentInfo: parentInfo, rootInfo: rootInfo, chain: chain}, nil
}

func (p *pinnedCanonical) verify() error {
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	defer func() { _ = syscall.Close(fd) }()
	for _, component := range p.chain {
		next, err := syscall.Openat(fd, component.name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fail("GLOBAL_ROOT_UNSAFE")
		}
		syscall.Close(fd)
		fd = next
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil || stat.Dev != component.dev || stat.Ino != component.ino || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			return fail("GLOBAL_ROOT_UNSAFE")
		}
	}
	if !samePinned(p.parentFD, p.parentInfo) || !samePinned(p.rootFD, p.rootInfo) {
		return fail("GLOBAL_ROOT_UNSAFE")
	}
	return nil
}

// Prepare writes only a durable PREPARED journal under administrator-controlled,
// private disposable roots. Caller-held exclusion and cooperative same-UID
// processes are preconditions: path checks cannot defeat hostile same-UID
// relocation between checks. This phase has no startup or publication authority.
// It refuses non-disposable roots even when a caller falsely claims exclusion.
func Prepare(ctx context.Context, roots globaladmission.Roots, exclusive bool) (Prepared, error) {
	return prepareWithHook(ctx, roots, exclusive, nil)
}

// beforeWrite exists only for the same-UID path-swap regression. It runs after
// FD pinning and before the first mutation; production passes nil.
func prepareWithHook(ctx context.Context, roots globaladmission.Roots, exclusive bool, beforeWrite func()) (Prepared, error) {
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
	controlExists, err := entryExistsAt(pin.rootFD, ".migration")
	if err != nil {
		return zero, err
	}
	generationsExists, err := entryExistsAt(pin.rootFD, "generations")
	if err != nil {
		return zero, err
	}
	if controlExists {
		controlFD, err := syscall.Openat(pin.rootFD, ".migration", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return zero, fail("GLOBAL_ROOT_UNSAFE")
		}
		defer syscall.Close(controlFD)
		receiptExists, err := entryExistsAt(controlFD, "receipt.json")
		if err != nil {
			return zero, err
		}
		if receiptExists {
			return zero, fail("GLOBAL_RECEIPT_PRESENT")
		}
		journalExists, err := entryExistsAt(controlFD, "journal.json")
		if err != nil {
			return zero, err
		}
		if !journalExists {
			return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		j, err := readJournalAt(controlFD)
		if err != nil {
			return zero, err
		}
		if j.Source != source {
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
		if !emptyDirectoryAt(pin.rootFD, "generations") {
			return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		if err := pin.verify(); err != nil {
			return zero, err
		}
		return Prepared{Generation: j.Generation, Source: j.Source, Phase: j.Phase}, nil
	}
	if generationsExists {
		return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
	}
	if err := rejectUnknownTargetAt(pin.rootFD); err != nil {
		return zero, err
	}
	if beforeWrite != nil {
		beforeWrite()
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	if err := syscall.Mkdirat(pin.rootFD, ".migration", 0700); err != nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Mkdirat(pin.rootFD, "generations", 0700); err != nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Fsync(pin.rootFD); err != nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	controlFD, err := syscall.Openat(pin.rootFD, ".migration", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	defer syscall.Close(controlFD)
	if err := pin.verify(); err != nil {
		return zero, err
	}
	id, err := newUUID()
	if err != nil {
		return zero, err
	}
	j := journal{Version: 1, Generation: id, Phase: "prepared", Source: source}
	if err := writeJournalAt(controlFD, j); err != nil {
		return zero, err
	}
	if err := pin.verify(); err != nil {
		return zero, err
	}
	return Prepared{Generation: id, Source: source, Phase: "prepared"}, nil
}

func disposableRoots(roots globaladmission.Roots) (os.FileInfo, os.FileInfo, error) {
	values := []string{roots.LegacySource, roots.CanonicalTarget, roots.UserData}
	parent := filepath.Dir(roots.CanonicalTarget)
	temp := filepath.Clean(os.TempDir())
	rel, err := filepath.Rel(temp, parent)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, nil, fail("GLOBAL_ROOT_UNSAFE")
	}
	for _, path := range values {
		if filepath.Dir(path) != parent || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, nil, fail("GLOBAL_ROOT_UNSAFE")
		}
	}
	var parentInfo, canonicalInfo os.FileInfo
	for _, path := range []string{parent, roots.CanonicalTarget, roots.UserData} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
			return nil, nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, nil, fail("GLOBAL_ROOT_UNSAFE")
		}
		if path == parent {
			parentInfo = info
		}
		if path == roots.CanonicalTarget {
			canonicalInfo = info
		}
	}
	return canonicalInfo, parentInfo, nil
}

func entryExistsAt(parentFD int, name string) (bool, error) {
	fd, err := syscall.Openat(parentFD, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fail("GLOBAL_ROOT_UNSAFE")
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT == syscall.S_IFLNK {
		return false, fail("GLOBAL_ROOT_UNSAFE")
	}
	return true, nil
}

func emptyDirectoryAt(parentFD int, name string) bool {
	fd, err := syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	entries, err := file.ReadDir(-1)
	return err == nil && len(entries) == 0
}

func rejectUnknownTargetAt(rootFD int) error {
	allowed := map[string]bool{"config.json": true, "models.json": true, "recent-projects.json": true, "mcp_config.json": true,
		"prompts": true, "skills": true, "skins": true, "logs": true, "metadata": true, "update": true}
	fd, err := syscall.Dup(rootFD)
	if err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	root := os.NewFile(uintptr(fd), "canonical")
	defer root.Close()
	entries, err := root.ReadDir(-1)
	if err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fail("GLOBAL_TARGET_UNKNOWN")
		}
	}
	return nil
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	const hex = "0123456789abcdef"
	var out [36]byte
	for i, b := range raw {
		position := i * 2
		if i >= 4 {
			position++
		}
		if i >= 6 {
			position++
		}
		if i >= 8 {
			position++
		}
		if i >= 10 {
			position++
		}
		out[position], out[position+1] = hex[b>>4], hex[b&15]
	}
	out[8], out[13], out[18], out[23] = '-', '-', '-', '-'
	return string(out[:]), nil
}

func writeJournalAt(controlFD int, value journal) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	temporary := "journal.json." + id + ".tmp"
	fd, err := syscall.Openat(controlFD, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	file := os.NewFile(uintptr(fd), temporary)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := file.Close(); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	// RENAME_NOREPLACE prevents an uncooperative same-UID writer from silently
	// replacing a journal after the absent check.
	oldName, _ := syscall.BytePtrFromString(temporary)
	newName, _ := syscall.BytePtrFromString("journal.json")
	_, _, errno := syscall.Syscall6(renameat2Trap, uintptr(controlFD), uintptr(unsafe.Pointer(oldName)), uintptr(controlFD), uintptr(unsafe.Pointer(newName)), 1, 0)
	if errno != 0 {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Fsync(controlFD); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}

func readJournalAt(controlFD int) (journal, error) {
	value, err := readJournalAnyAt(controlFD)
	if err != nil {
		return journal{}, err
	}
	if value.Phase != "prepared" {
		return journal{}, fail("GLOBAL_JOURNAL_INVALID")
	}
	return value, nil
}

func readJournalAnyAt(controlFD int) (journal, error) {
	return readJournalNamedAt(controlFD, "journal.json")
}

func readJournalNamedAt(controlFD int, name string) (journal, error) {
	var zero journal
	fd, err := syscall.Openat(controlFD, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return zero, fail("GLOBAL_JSON_INVALID")
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return zero, fail("GLOBAL_JSON_INVALID")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return zero, fail("GLOBAL_JSON_INVALID")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return zero, fail("GLOBAL_JSON_INVALID")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&zero); err != nil {
		return journal{}, fail("GLOBAL_JSON_INVALID")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return journal{}, fail("GLOBAL_JSON_INVALID")
	}
	if zero.Version != 1 || len(zero.Generation) != 36 || (zero.Phase != "prepared" && zero.Phase != "verified") ||
		(zero.Source != "legacy" && zero.Source != "canonical" && zero.Source != "empty") {
		return journal{}, fail("GLOBAL_JOURNAL_INVALID")
	}
	for i, c := range zero.Generation {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return journal{}, fail("GLOBAL_JOURNAL_INVALID")
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return journal{}, fail("GLOBAL_JOURNAL_INVALID")
		}
	}
	return zero, nil
}
