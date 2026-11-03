package projectfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const MaxVisibleTreeEntries = 4096
const MaxVisibleTreeDepth = 16

var ErrTreeLimit = errors.New("PROJECT_TREE_LIMIT")

// VisibleNode is the browser's fs:list-dir shape. Empty directories retain an
// explicit children array; files omit it. Hidden project storage is omitted.
type VisibleNode struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	IsDir    bool           `json:"isDir"`
	Children *[]VisibleNode `json:"children,omitempty"`
}

// ListVisibleTree enumerates a bounded subtree through the pinned project
// root. The caller must derive relative from an already admitted project
// context; a browser pathname is never opened here. No links, hardlinks or
// special visible entries can be represented as successful tree nodes.
func (p *Project) ListVisibleTree(ctx context.Context, relative string, maxEntries, maxDepth int) ([]VisibleNode, error) {
	if p == nil || ctx == nil || maxEntries < 1 || maxEntries > MaxVisibleTreeEntries || maxDepth < 1 || maxDepth > MaxVisibleTreeDepth {
		return nil, ErrInvalidPath
	}
	if relative != "." && !validRelative(relative) {
		return nil, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	current := p.root
	var opened []*os.Root
	defer func() {
		for _, root := range opened {
			_ = root.Close()
		}
	}()
	if relative != "." {
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			next, _, err := openChild(current, part)
			if err != nil {
				return nil, err
			}
			opened = append(opened, next)
			current = next
		}
	}
	count := 0
	result, err := p.readVisibleTree(ctx, current, relative, 0, &count, maxEntries, maxDepth)
	if err != nil {
		return nil, err
	}
	if err := p.checkIdentityLocked(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *Project) readVisibleTree(ctx context.Context, root *os.Root, relative string, depth int, count *int, maxEntries, maxDepth int) ([]VisibleNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth > maxDepth {
		return nil, ErrTreeLimit
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	before, err := directory.Stat()
	if err != nil || !directoryInfo(before) {
		_ = directory.Close()
		return nil, ErrUnsafeDirectory
	}
	remaining := maxEntries - *count
	entries, readErr := directory.ReadDir(remaining + 1)
	after, statErr := directory.Stat()
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if statErr != nil || closeErr != nil || !sameDirectoryVersion(before, after) {
		return nil, ErrChanged
	}
	if len(entries) > remaining {
		return nil, ErrTreeLimit
	}
	*count += len(entries)
	output := make([]VisibleNode, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !validRelative(name) || filepath.Base(name) != name {
			return nil, ErrUnsafeFile
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		beforeEntry, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		childRelative := name
		if relative != "." {
			childRelative = filepath.Join(relative, name)
		}
		node := VisibleNode{Name: name, Path: filepath.Join(p.path, childRelative)}
		switch {
		case directoryInfo(beforeEntry):
			if depth >= maxDepth {
				return nil, ErrTreeLimit
			}
			child, childInfo, err := openChild(root, name)
			if err != nil {
				return nil, err
			}
			children, readErr := p.readVisibleTree(ctx, child, childRelative, depth+1, count, maxEntries, maxDepth)
			closeErr := child.Close()
			afterEntry, statErr := root.Lstat(name)
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil || statErr != nil || !sameDirectoryVersion(beforeEntry, childInfo) || !sameDirectoryVersion(beforeEntry, afterEntry) {
				return nil, ErrChanged
			}
			node.IsDir = true
			node.Children = &children
		case beforeEntry.Mode().IsRegular():
			file, info, err := openRegular(root, name)
			if err != nil {
				return nil, err
			}
			closeErr := file.Close()
			afterEntry, statErr := root.Lstat(name)
			if closeErr != nil || statErr != nil || !sameVersion(info, afterEntry) {
				return nil, ErrChanged
			}
		default:
			return nil, ErrUnsafeFile
		}
		output = append(output, node)
	}
	collator := collate.New(language.SimplifiedChinese)
	sort.SliceStable(output, func(i, j int) bool {
		if output[i].IsDir != output[j].IsDir {
			return output[i].IsDir
		}
		if comparison := collator.CompareString(output[i].Name, output[j].Name); comparison != 0 {
			return comparison < 0
		}
		return output[i].Name < output[j].Name
	})
	return output, nil
}

func directoryInfo(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func sameDirectoryVersion(before, after os.FileInfo) bool {
	return directoryInfo(before) && directoryInfo(after) && os.SameFile(before, after) &&
		before.Mode() == after.Mode() && before.Size() == after.Size() &&
		before.ModTime().Equal(after.ModTime()) && sameChangeTime(before, after)
}
