package projectfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const MaxLibraryEntries = 10_000

var ErrLibraryLimit = errors.New("PROJECT_LIBRARY_LIMIT")

// ListedProject describes one canonical immediate child of a trusted library.
// Listing does not grant project opening, SQLite admission, or preview access.
// Visibility is deliberately absent: the original /api/library includes hidden
// books, and a separate visibility store must decide other views.
type ListedProject struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	ProjectID string `json:"projectId"`
	UpdatedAt string `json:"updatedAt"`
}

// ListCanonicalProjects bounds directory enumeration and checks each direct
// child's canonical manifest and database file. Invalid or incomplete children
// are omitted, matching the browser library inventory.
// A changed library root fails the entire call rather than returning a partial
// list. Ties in the millisecond timestamp use name order for stable output.
func (l *Library) ListCanonicalProjects(ctx context.Context, maxEntries int) ([]ListedProject, error) {
	if l == nil || ctx == nil || maxEntries < 1 || maxEntries > MaxLibraryEntries {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if err := l.checkLocked(); err != nil {
		l.mu.Unlock()
		return nil, err
	}
	directory, err := l.root.Open(".")
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	opened, err := directory.Stat()
	if err != nil || !os.SameFile(l.info, opened) {
		_ = directory.Close()
		l.mu.Unlock()
		return nil, ErrChanged
	}
	entries, readErr := directory.ReadDir(maxEntries + 1)
	closeErr := directory.Close()
	if err := l.checkLocked(); err != nil {
		l.mu.Unlock()
		return nil, err
	}
	l.mu.Unlock()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maxEntries {
		return nil, ErrLibraryLimit
	}

	output := make([]ListedProject, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !validRelative(name) || filepath.Base(name) != name || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		l.mu.Lock()
		listed, err := l.libraryEntryLocked(ctx, name)
		rootErr := l.checkLocked()
		l.mu.Unlock()
		if rootErr != nil {
			return nil, rootErr
		}
		if err == nil {
			output = append(output, listed)
		}
	}
	l.mu.Lock()
	err = l.checkLocked()
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(output, func(i, j int) bool {
		if output[i].UpdatedAt != output[j].UpdatedAt {
			return output[i].UpdatedAt > output[j].UpdatedAt
		}
		return output[i].Name < output[j].Name
	})
	return output, nil
}

// libraryEntryLocked inspects only the canonical manifest and database file.
// This matches the original inventory even when an old migration marker is
// present; admission of that project remains a separate, stricter operation.
func (l *Library) libraryEntryLocked(ctx context.Context, name string) (ListedProject, error) {
	if err := ctx.Err(); err != nil {
		return ListedProject{}, err
	}
	if err := l.checkLocked(); err != nil {
		return ListedProject{}, err
	}
	root, rootInfo, err := openChild(l.root, name)
	if err != nil {
		return ListedProject{}, err
	}
	defer root.Close()
	storage, storageInfo, err := openChild(root, StorageDirectory)
	if err != nil {
		return ListedProject{}, err
	}
	defer storage.Close()
	raw, manifestInfo, err := readBounded(storage, ManifestName, manifestLimit)
	if err != nil {
		return ListedProject{}, err
	}
	manifest, err := ParseManifest(raw)
	if err != nil {
		return ListedProject{}, err
	}
	file, info, err := openRegular(storage, DatabaseName)
	if err != nil {
		return ListedProject{}, err
	}
	if err := file.Close(); err != nil {
		return ListedProject{}, err
	}
	rootNow, err := l.root.Lstat(name)
	if err != nil || !os.SameFile(rootInfo, rootNow) {
		return ListedProject{}, ErrChanged
	}
	storageNow, err := root.Lstat(StorageDirectory)
	if err != nil || !os.SameFile(storageInfo, storageNow) {
		return ListedProject{}, ErrChanged
	}
	manifestNow, err := storage.Lstat(ManifestName)
	if err != nil || !sameVersion(manifestInfo, manifestNow) {
		return ListedProject{}, ErrChanged
	}
	databaseNow, err := storage.Lstat(DatabaseName)
	if err != nil || !sameVersion(info, databaseNow) {
		return ListedProject{}, ErrChanged
	}
	return ListedProject{
		Name: name, Path: filepath.Join(l.path, name), ProjectID: manifest.ProjectID,
		UpdatedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
	}, nil
}
