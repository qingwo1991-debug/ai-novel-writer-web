//go:build lance_native && cgo

package projectaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/lancenative"
)

func nativeKnowledgeSupported() bool { return true }

// inspectNativeKnowledge has no original-author path input. All database work is
// on a complete, independently proven private copy. The caller holds the legacy
// flock and must exclude noncooperating writers across the whole preflight.
// Snapshot contains private rows only for in-process link validation, never Report.
func inspectNativeKnowledge(ctx context.Context, stage *stagedKnowledge, config KnowledgeConfig) (snapshot knowledgedomain.Snapshot, summary knowledgedomain.Summary, err error) {
	if ctx == nil || stage == nil || stage.root == nil || stage.parent == nil || !config.Native {
		return snapshot, summary, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return snapshot, summary, err
	}
	registry, err := readNativeRegistry(ctx, stage.root, config.Metadata.MaxMetadataBytes)
	if err != nil {
		return snapshot, summary, err
	}
	snapshot.Registry = registry
	lance, statErr := stage.root.Lstat("lancedb")
	if errors.Is(statErr, fs.ErrNotExist) {
		summary, err = knowledgedomain.Validate(ctx, snapshot, nativeDomainBudget())
		if err != nil {
			return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, redact(err)
		}
		return snapshot, summary, nil // Never connect to or create an absent Lance root.
	}
	if statErr != nil || !lance.IsDir() || lance.Mode()&os.ModeSymlink != 0 {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, ErrFilesystem
	}
	output, err := newNativeOutput(stage.parent)
	if err != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, err
	}
	defer func() {
		if cleanupErr := output.cleanup(); cleanupErr != nil {
			err = errors.Join(err, ErrCleanup)
		}
		if err != nil {
			snapshot = knowledgedomain.Snapshot{}
			summary = knowledgedomain.Summary{}
		}
	}()
	manifest, nativeErr := lancenative.Export(ctx, filepath.Join(stage.root.Name(), "lancedb"), output.root.Name())
	if nativeErr != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, redact(nativeErr)
	}
	raw, readErr := lancenative.ReadDomain(ctx, output.root.Name())
	if readErr != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, redact(readErr)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&snapshot); decodeErr != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, ErrFilesystem
	}
	if decoder.Decode(new(any)) != io.EOF {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, ErrFilesystem
	}
	if err := validateNativeManifest(manifest, snapshot); err != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, err
	}
	snapshot.Registry = registry
	summary, err = knowledgedomain.Validate(ctx, snapshot, nativeDomainBudget())
	if err != nil {
		return knowledgedomain.Snapshot{}, knowledgedomain.Summary{}, redact(err)
	}
	return snapshot, summary, nil
}

// The export manifest and typed JSON sidecar are two observations from one
// pinned native snapshot. Both must identify the same ordered tables, versions
// and row counts before the JSON can be used as author knowledge evidence.
// The native library writes these independently; accepting just one would let
// an incomplete or stale sidecar masquerade as a complete export.
func validateNativeManifest(manifest lancenative.Manifest, snapshot knowledgedomain.Snapshot) error {
	if len(manifest.Tables) != len(snapshot.Tables) || len(manifest.Tables) > knowledgedomain.HardMaxTables {
		return ErrFilesystem
	}
	seen := make(map[string]bool, len(manifest.Tables))
	for i, entry := range manifest.Tables {
		table := snapshot.Tables[i]
		if entry.Name == "" || seen[entry.Name] || entry.Version == 0 ||
			entry.File != entry.Name+".arrows" || entry.Name != table.Name ||
			entry.Version != table.Version || entry.Rows != uint64(len(table.Records)) {
			return ErrFilesystem
		}
		seen[entry.Name] = true
	}
	return nil
}
func nativeDomainBudget() knowledgedomain.Budget {
	return knowledgedomain.Budget{MaxTables: 32, MaxRows: 10000, MaxCells: 200000, MaxTextBytes: 32 * 1024 * 1024}
}
func readNativeRegistry(ctx context.Context, root *os.Root, max int64) (json.RawMessage, error) {
	info, err := root.Lstat("embedding-spaces.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrFilesystem
	}
	file, err := root.OpenFile("embedding-spaces.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrFilesystem
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, ErrFilesystem
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, max+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(raw)) > max {
		return nil, ErrFilesystem
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// The native implementation requires an exclusive empty private output. Thus
// fixed-whitelist regular outputs observed after it settles belong to this call,
// including partial results on failure. This is not hostile same-UID provenance
// protection: the preflight's continuous quiescence requirement also covers output.
// Cleanup pins every observed inode until after removal, never follows links,
// never recurses, and preserves unknown/replacement objects with ErrCleanup.
const nativeOutputName = "native-knowledge"

type nativeOutput struct {
	parent, root *os.Root
	info         os.FileInfo
}

func newNativeOutput(parent *os.Root) (output *nativeOutput, err error) {
	if err := parent.Mkdir(nativeOutputName, 0700); err != nil {
		return nil, ErrFilesystem
	}
	output = &nativeOutput{parent: parent}
	owned := output
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.cleanup())
			output = nil
		}
	}()
	output.info, err = parent.Lstat(nativeOutputName)
	if err != nil {
		return output, ErrCleanup
	}
	output.root, err = parent.OpenRoot(nativeOutputName)
	if err != nil {
		return output, ErrFilesystem
	}
	opened, err := output.root.Stat(".")
	if err != nil || !os.SameFile(output.info, opened) {
		return output, ErrFilesystem
	}
	return output, nil
}
func nativeOutputFile(name string) bool {
	if name == "domain.json" || name == "chunks.arrows" || name == "documents.arrows" {
		return true
	}
	suffix, ok := strings.CutPrefix(name, "chunks__space_")
	if !ok {
		return false
	}
	digits, ok := strings.CutSuffix(suffix, ".arrows")
	if !ok || len(digits) == 0 || len(digits) > 20 {
		return false
	}
	for _, digit := range []byte(digits) {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
func privateNativeFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}
func (output *nativeOutput) cleanup() error {
	if output == nil {
		return nil
	}
	failed := false
	type ownedFile struct {
		name string
		info os.FileInfo
		file *os.File
	}
	var owned []ownedFile
	if output.root != nil {
		directory, err := output.root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			failed = true
		} else {
			entries, readErr := directory.ReadDir(34)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				failed = true
			}
			if len(entries) > 33 {
				failed = true
			}
			for _, entry := range entries {
				name := entry.Name()
				if !nativeOutputFile(name) {
					failed = true
					continue
				}
				before, err := output.root.Lstat(name)
				if err != nil || !privateNativeFile(before) {
					failed = true
					continue
				}
				file, err := output.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
				if err != nil {
					failed = true
					continue
				}
				pinned, err := file.Stat()
				if err != nil || !privateNativeFile(pinned) || !os.SameFile(before, pinned) {
					file.Close()
					failed = true
					continue
				}
				owned = append(owned, ownedFile{name, pinned, file})
			}
			if err := directory.Close(); err != nil {
				failed = true
			}
		}
		for _, file := range owned {
			named, err := output.root.Lstat(file.name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil || !privateNativeFile(named) || !os.SameFile(file.info, named) {
				failed = true
				continue
			}
			if err := output.root.Remove(file.name); err != nil {
				failed = true
			}
		}
		for _, file := range owned {
			if err := file.file.Close(); err != nil {
				failed = true
			}
		}
		if err := output.root.Close(); err != nil {
			failed = true
		}
	}
	named, err := output.parent.Lstat(nativeOutputName)
	if err != nil || output.info == nil || !os.SameFile(output.info, named) {
		failed = true
	} else if err := output.parent.Remove(nativeOutputName); err != nil {
		failed = true
	}
	if failed {
		return ErrCleanup
	}
	return nil
}
