//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrCatalogRecoveryUnverified = errors.New("PROJECT_PUBLICATION_RECOVERY_UNVERIFIED")

// SwitchedReadback verifies the newly visible source and the edited draft
// against the journal proof. A switched marker alone is never clearance.
// Implementations must not call back into the catalog: its mutex is held.
type SwitchedReadback func(context.Context, string, string, projectpublish.JournalRecovery) error

// CatalogJournalRecoveryCheck supplies a fail-closed catalog callback. The
// private stage parent is dedicated to publication artifacts. Every entry is
// inspected on each call; an unrelated or malformed journal blocks the whole
// catalog rather than being guessed to belong to a different project. A
// publisher must HoldRecovery before creating or exchanging a stage in this
// process. Neither this callback nor a switched marker performs recovery.
func CatalogJournalRecoveryCheck(ctx context.Context, held *writerlock.Lock, dataRoot, stageParent string, readback SwitchedReadback) (projectcatalog.RecoveryCheck, error) {
	if ctx == nil || held == nil || !catalogRecoveryPath(dataRoot) || !catalogRecoveryPath(stageParent) {
		return nil, ErrCatalogRecoveryUnverified
	}
	if err := held.VerifyLegacy(dataRoot); err != nil {
		return nil, err
	}
	parentInfo, err := catalogRecoveryPrivateDirectory(stageParent)
	if err != nil {
		return nil, ErrCatalogRecoveryUnverified
	}
	return func(projectPath, projectID string) error {
		if projectID == "" || !catalogRecoveryPath(projectPath) || !catalogRecoveryChild(dataRoot, projectPath) {
			return ErrCatalogRecoveryUnverified
		}
		storagePath := filepath.Join(projectPath, projectfs.StorageDirectory)
		if stageParent == storagePath || catalogRecoveryChild(storagePath, stageParent) || catalogRecoveryChild(stageParent, storagePath) {
			return ErrCatalogRecoveryUnverified
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrCatalogRecoveryUnverified, err)
		}
		if err := held.VerifyLegacy(dataRoot); err != nil {
			return errors.Join(ErrCatalogRecoveryUnverified, err)
		}
		current, err := catalogRecoveryPrivateDirectory(stageParent)
		if err != nil || !os.SameFile(parentInfo, current) {
			return ErrCatalogRecoveryUnverified
		}
		root, err := os.OpenRoot(stageParent)
		if err != nil {
			return ErrCatalogRecoveryUnverified
		}
		defer root.Close()
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(parentInfo, opened) {
			return ErrCatalogRecoveryUnverified
		}
		file, err := root.Open(".")
		if err != nil {
			return ErrCatalogRecoveryUnverified
		}
		entries, readErr := file.ReadDir(-1)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return ErrCatalogRecoveryUnverified
		}
		prepared := make(map[string]bool)
		markers := make(map[string]bool)
		for _, entry := range entries {
			name := entry.Name()
			info, err := root.Lstat(name)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return ErrCatalogRecoveryUnverified
			}
			switch {
			case info.IsDir() && catalogRecoveryStageName(name) && info.Mode().Perm() == 0700:
			case info.Mode().IsRegular() && catalogRecoveryJournalName(name):
				prepared[name] = true
			case info.Mode().IsRegular() && strings.HasSuffix(name, ".switched") && catalogRecoveryJournalName(strings.TrimSuffix(name, ".switched")):
				markers[strings.TrimSuffix(name, ".switched")] = true
			default:
				return ErrCatalogRecoveryUnverified
			}
		}
		for name := range markers {
			if !prepared[name] {
				return ErrCatalogRecoveryUnverified
			}
		}
		for name := range prepared {
			recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, dataRoot, projectID, projectPath, stageParent, name)
			if err != nil {
				return ErrCatalogRecoveryUnverified
			}
			if err := catalogRecoveryAcceptSwitched(ctx, projectPath, projectID, recovery, readback); err != nil {
				return err
			}
		}
		last, err := catalogRecoveryPrivateDirectory(stageParent)
		if err != nil || !os.SameFile(parentInfo, last) {
			return ErrCatalogRecoveryUnverified
		}
		if err := held.VerifyLegacy(dataRoot); err != nil {
			return errors.Join(ErrCatalogRecoveryUnverified, err)
		}
		return nil
	}, nil
}

func catalogRecoveryAcceptSwitched(ctx context.Context, projectPath, projectID string, recovery projectpublish.JournalRecovery, readback SwitchedReadback) error {
	if recovery.Class != projectpublish.JournalSwitched || readback == nil || recovery.ProjectPath != projectPath || recovery.JournalID == "" {
		return ErrCatalogRecoveryUnverified
	}
	if err := readback(ctx, projectPath, projectID, recovery); err != nil {
		return errors.Join(ErrCatalogRecoveryUnverified, err)
	}
	return nil
}

func catalogRecoveryPath(path string) bool {
	return path != "/" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\\\x00")
}

func catalogRecoveryChild(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func catalogRecoveryPrivateDirectory(path string) (fs.FileInfo, error) {
	if !catalogRecoveryPath(path) {
		return nil, ErrCatalogRecoveryUnverified
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" || component == "." || component == ".." {
			return nil, ErrCatalogRecoveryUnverified
		}
		current = filepath.Join(current, component)
		ancestor, err := os.Lstat(current)
		if err != nil || !ancestor.IsDir() || ancestor.Mode()&os.ModeSymlink != 0 {
			return nil, ErrCatalogRecoveryUnverified
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return nil, ErrCatalogRecoveryUnverified
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrCatalogRecoveryUnverified
	}
	return info, nil
}

func catalogRecoveryJournalName(name string) bool {
	const prefix, suffix = "novel-exchange-", ".prepared.json"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) || len(name) != len(prefix)+32+len(suffix) {
		return false
	}
	return catalogRecoveryHex(name[len(prefix) : len(prefix)+32])
}

func catalogRecoveryStageName(name string) bool {
	const prefix = "novel-private-publish-"
	return strings.HasPrefix(name, prefix) && len(name) == len(prefix)+32 && catalogRecoveryHex(name[len(prefix):])
}

func catalogRecoveryHex(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}
