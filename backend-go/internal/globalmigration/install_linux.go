//go:build linux && amd64

package globalmigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
)

// CompleteVerified completes only a journal-owned migration in private,
// disposable roots. Its Ready value is evidence for tests, not an activation
// authority. The caller must hold exclusion and all same-UID processes must
// cooperate throughout this call and subsequent use of the returned root.
func CompleteVerified(ctx context.Context, roots globaladmission.Roots, exclusive bool) (globaladmission.Ready, error) {
	var zero globaladmission.Ready
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
	// Once a completed receipt exists, the original migration no longer
	// compares source bytes. Preserve that behavior on a verified restart.
	if _, err := os.Lstat(filepath.Join(roots.CanonicalTarget, ".migration", "receipt.json")); err == nil {
		return globaladmission.VerifyCompleted(ctx, roots)
	} else if !errors.Is(err, os.ErrNotExist) {
		return zero, fail("GLOBAL_ROOT_UNSAFE")
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
		return zero, fail("GLOBAL_ROOT_UNSAFE")
	}
	defer syscall.Close(controlFD)
	genFD, err := stageOpenDir(pin.rootFD, "generations")
	if err != nil {
		return zero, fail("GLOBAL_ROOT_UNSAFE")
	}
	defer syscall.Close(genFD)
	j, err := readJournalAnyAt(controlFD)
	if err != nil {
		return zero, err
	}
	if j.Phase != "verified" || j.Source != source {
		return zero, fail("GLOBAL_SOURCE_CHANGED")
	}
	legacyFD := -1
	if legacyInfo != nil {
		legacyFD, err = stageOpenDir(pin.parentFD, filepath.Base(roots.LegacySource))
		if err != nil || !samePinned(legacyFD, legacyInfo) {
			if err == nil {
				syscall.Close(legacyFD)
			}
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
		defer syscall.Close(legacyFD)
	}
	sourceFD := pin.rootFD
	if j.Source == "legacy" {
		if legacyFD < 0 {
			return zero, fail("GLOBAL_SOURCE_CHANGED")
		}
		sourceFD = legacyFD
	}
	before, err := stageFiles(ctx, sourceFD, false)
	if err != nil {
		return zero, err
	}
	stagingName := j.Generation + ".staging"
	stagingExists, err := entryExistsAt(genFD, stagingName)
	if err != nil {
		return zero, err
	}
	installedExists, err := entryExistsAt(genFD, j.Generation)
	if err != nil {
		return zero, err
	}
	if stagingExists == installedExists {
		// The original can recreate an absent empty staging directory. This
		// incomplete private writer instead stops for explicit adjudication.
		return zero, fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
	}
	wantName := stagingName
	if installedExists {
		wantName = j.Generation
	}
	if err := stageCheckGenerations(genFD, wantName); err != nil {
		return zero, err
	}
	dataFD, err := stageOpenDir(genFD, wantName)
	if err != nil {
		return zero, fail("GLOBAL_ROOT_UNSAFE")
	}
	defer syscall.Close(dataFD)
	if err := installVerify(ctx, pin, roots, legacyFD, controlFD, genFD, dataFD, wantName, sourceFD, before); err != nil {
		return zero, err
	}
	if stagingExists {
		if err := stageSyncDirs(dataFD); err != nil {
			return zero, err
		}
		if err := syscall.Fsync(genFD); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		if err := installVerify(ctx, pin, roots, legacyFD, controlFD, genFD, dataFD, stagingName, sourceFD, before); err != nil {
			return zero, err
		}
		if err := stageRenameNoReplace(genFD, stagingName, genFD, j.Generation); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		if err := syscall.Fsync(genFD); err != nil {
			return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
		}
	}
	if err := installVerify(ctx, pin, roots, legacyFD, controlFD, genFD, dataFD, j.Generation, sourceFD, before); err != nil {
		return zero, err
	}
	count, err := installUnknownCount(pin.rootFD, legacyFD)
	if err != nil {
		return zero, err
	}
	var required []string
	for _, name := range [...]string{"config.json", "models.json", "recent-projects.json", "mcp_config.json", "prompts", "skills", "skins"} {
		exists, err := entryExistsAt(dataFD, name)
		if err != nil {
			return zero, err
		}
		if exists {
			required = append(required, name)
		}
	}
	if required == nil {
		required = []string{}
	}
	receipt := struct {
		Version               int      `json:"version"`
		Generation            string   `json:"generation"`
		Completed             bool     `json:"completed"`
		PreservedUnknownCount int      `json:"preservedUnknownCount"`
		RequiredObjects       []string `json:"requiredObjects"`
	}{1, j.Generation, true, count, required}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return zero, fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := installVerify(ctx, pin, roots, legacyFD, controlFD, genFD, dataFD, j.Generation, sourceFD, before); err != nil {
		return zero, err
	}
	current, err := readJournalAnyAt(controlFD)
	if err != nil || current != j {
		return zero, fail("GLOBAL_JOURNAL_INVALID")
	}
	if err := installWriteReceipt(controlFD, raw, pin); err != nil {
		return zero, err
	}
	return globaladmission.VerifyCompleted(ctx, roots)
}

func installVerify(ctx context.Context, pin *pinnedCanonical, roots globaladmission.Roots, legacyFD, controlFD, genFD, dataFD int, name string, sourceFD int, before map[string]stageDigest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pin.verify(); err != nil {
		return err
	}
	if err := stageVerifyDirectory(pin.rootFD, ".migration", controlFD); err != nil {
		return err
	}
	if err := stageVerifyDirectory(pin.rootFD, "generations", genFD); err != nil {
		return err
	}
	if err := stageVerifyDirectory(genFD, name, dataFD); err != nil {
		return err
	}
	if legacyFD >= 0 {
		if err := stageVerifyDirectory(pin.parentFD, filepath.Base(roots.LegacySource), legacyFD); err != nil {
			return err
		}
	}
	if err := stageParity(ctx, sourceFD, dataFD, before, "verified"); err != nil {
		return err
	}
	if err := globaladmission.ValidateGeneration("/proc/self/fd/" + strconv.Itoa(dataFD)); err != nil {
		return err
	}
	return nil
}

func installUnknownCount(canonicalFD, legacyFD int) (int, error) {
	countRoot := func(fd int) (int, error) {
		if fd < 0 {
			return 0, nil
		}
		names, err := stageNames(fd)
		if err != nil {
			return 0, err
		}
		count := 0
		for _, name := range names {
			if !stageObjects[name] && name != ".migration" && name != "generations" {
				count++
			}
		}
		return count, nil
	}
	canonical, err := countRoot(canonicalFD)
	if err != nil {
		return 0, err
	}
	legacy, err := countRoot(legacyFD)
	return canonical + legacy, err
}

func installWriteReceipt(controlFD int, raw []byte, pin *pinnedCanonical) error {
	if exists, err := entryExistsAt(controlFD, "receipt.json"); err != nil {
		return err
	} else if exists {
		return fail("GLOBAL_RECEIPT_PRESENT")
	}
	names, err := stageNames(controlFD)
	if err != nil {
		return err
	}
	pending := ""
	for _, name := range names {
		if name == "journal.json" {
			continue
		}
		if !strings.HasPrefix(name, "receipt.json.") || !stageTempSuffix.MatchString(name) || pending != "" {
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		pending = name
	}
	if pending != "" {
		fd, err := syscall.Openat(controlFD, pending, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		file := os.NewFile(uintptr(fd), pending)
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(raw)) {
			file.Close()
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
			file.Close()
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		got, err := io.ReadAll(io.LimitReader(file, int64(len(raw))+1))
		if err == nil && bytes.Equal(got, raw) {
			err = file.Sync()
		} else if err == nil {
			err = fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fail("GLOBAL_PARTIAL_TARGET_UNKNOWN")
		}
	} else {
		id, err := newUUID()
		if err != nil {
			return err
		}
		pending = "receipt.json." + id + ".tmp"
		fd, err := syscall.Openat(controlFD, pending, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
		file := os.NewFile(uintptr(fd), pending)
		if _, err = file.Write(raw); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fail("GLOBAL_MIGRATION_IO_FAILED")
		}
	}
	if err := syscall.Fsync(controlFD); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := pin.verify(); err != nil {
		return err
	}
	if err := stageRenameNoReplace(controlFD, pending, controlFD, "receipt.json"); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	if err := syscall.Fsync(controlFD); err != nil {
		return fail("GLOBAL_MIGRATION_IO_FAILED")
	}
	return nil
}
