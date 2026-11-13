package promptskills

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
)

func isSymlink(info fs.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }

var ErrPackageSource = errors.New("SOURCE_INVALID")
var ErrPackageSymlink = errors.New("SYMLINK")
var ErrPackageMissing = errors.New("SKILL_MD_MISSING")
var ErrPackageInvalid = errors.New("SKILL_MD_INVALID")
var ErrPackageLarge = errors.New("FOLDER_TOO_LARGE")

// PackageRoot is an explicitly trusted staging root, not browser authority.
// The upload/grant owner must map an authenticated owned upload to this root;
// a renderer-supplied server path must never be passed to NewPackageRoot.
// No public local install API is exposed until that owner is implemented.
type PackageRoot struct {
	mu     sync.Mutex
	root   *ownedDir
	closed bool
}

func NewPackageRoot(trustedRoot string) (*PackageRoot, error) {
	d, err := openOwnedRoot(trustedRoot)
	if err != nil {
		return nil, err
	}
	return &PackageRoot{root: d}, nil
}
func (p *PackageRoot) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	err := p.root.close()
	p.root = nil
	return err
}

type LoadedPackage struct {
	Raw           string        `json:"raw"`
	Inspection    Inspection    `json:"inspection"`
	BundledFiles  []BundledFile `json:"bundledFiles"`
	SkippedFiles  []string      `json:"skippedFiles"`
	SourceName    string        `json:"sourceName"`
	ContentSHA256 string        `json:"contentSha256"`
}

func safeRelative(relative string) bool {
	if relative == "" {
		return false
	}
	for _, part := range strings.Split(relative, "/") {
		if !segmentPattern.MatchString(part) || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func packageDigest(raw string, files []BundledFile) (string, error) {
	values := []any{raw}
	for _, f := range files {
		values = append(values, f)
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(values); err != nil {
		return "", err
	}
	data := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	data = bytes.ReplaceAll(data, []byte(`\u2028`), []byte("\u2028"))
	data = bytes.ReplaceAll(data, []byte(`\u2029`), []byte("\u2029"))
	return digest(data), nil
}
func (p *PackageRoot) Load(relative, kind string) (LoadedPackage, error) {
	if !safeRelative(relative) || (kind != "file" && kind != "folder") {
		return LoadedPackage{}, ErrPackageSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return LoadedPackage{}, ErrClosed
	}
	sourceName := path.Base(relative)
	loaded := LoadedPackage{BundledFiles: []BundledFile{}, SkippedFiles: []string{}, SourceName: sourceName}
	fallback := ""
	if kind == "file" {
		if !rx(`(?i)\.(?:md|markdown)$`, relative) {
			return loaded, ErrPackageSource
		}
		data, err := readRelative(p.root, relative, MaxSkillBytes)
		if err != nil {
			return loaded, ErrPackageSource
		}
		if !validText(data) {
			return loaded, ErrPackageInvalid
		}
		loaded.Raw = string(data)
		fallback = strings.TrimSuffix(sourceName, path.Ext(sourceName))
		if strings.EqualFold(fallback, "skill") {
			fallback = path.Base(path.Dir(relative))
		}
		loaded.Inspection = InspectLocal(loaded.Raw, nil, fallback)
	} else {
		cursor := p.root
		defer func() {
			if cursor != p.root {
				cursor.close()
			}
		}()
		for _, part := range strings.Split(relative, "/") {
			next, err := openChildDir(cursor, part, false, true)
			if err != nil {
				return loaded, ErrPackageSource
			}
			if cursor != p.root {
				cursor.close()
			}
			cursor = next
		}
		files := []string{}
		visited := 0
		var walk func(*ownedDir, string, int) error
		walk = func(dir *ownedDir, prefix string, depth int) error {
			items, err := entries(dir)
			if err != nil {
				return err
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Name() < items[j].Name() })
			for _, item := range items {
				if strings.HasPrefix(item.Name(), ".") {
					continue
				}
				visited++
				if visited > MaxFolderEntries {
					return ErrPackageLarge
				}
				info, err := statAt(dir, item.Name())
				if err != nil {
					return err
				}
				if isSymlink(info) {
					return ErrPackageSymlink
				}
				relative := item.Name()
				if prefix != "" {
					relative = prefix + "/" + relative
				}
				if info.IsDir() {
					if depth >= MaxFolderDepth {
						return ErrPackageLarge
					}
					child, err := openChildDir(dir, item.Name(), false, true)
					if err != nil {
						return err
					}
					err = walk(child, relative, depth+1)
					child.close()
					if err != nil {
						return err
					}
				} else {
					files = append(files, relative)
				}
			}
			return nil
		}
		if err := walk(cursor, "", 1); err != nil {
			return loaded, err
		}
		skillFile := ""
		for _, f := range files {
			if strings.EqualFold(f, "skill.md") {
				skillFile = f
				break
			}
		}
		if skillFile == "" {
			return loaded, ErrPackageMissing
		}
		data, err := readRelative(cursor, skillFile, MaxSkillBytes)
		if err != nil || !validText(data) {
			return loaded, ErrPackageInvalid
		}
		loaded.Raw = string(data)
		record := map[string]string{}
		loaded.Inspection = InspectLocal(loaded.Raw, func(relative string) (string, bool) {
			if !validBundlePath(relative) {
				return "", false
			}
			data, err := readRelative(cursor, relative, MaxSkillBytes)
			if err != nil || !validText(data) {
				return "", false
			}
			record[relative] = string(data)
			return string(data), true
		}, sourceName)
		included := map[string]bool{}
		for _, f := range loaded.Inspection.BundledFiles {
			loaded.BundledFiles = append(loaded.BundledFiles, BundledFile{Path: f, Content: record[f]})
			included[strings.ToLower(f)] = true
		}
		for _, f := range files {
			if f != skillFile && !included[strings.ToLower(f)] {
				loaded.SkippedFiles = append(loaded.SkippedFiles, f)
			}
		}
	}
	if !ValidName(loaded.Inspection.Metadata.Name) {
		return loaded, ErrPackageSource
	}
	hash, err := packageDigest(loaded.Raw, loaded.BundledFiles)
	if err != nil {
		return loaded, err
	}
	loaded.ContentSHA256 = hash
	return loaded, nil
}
