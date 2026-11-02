// Package knowledgecopy validates JSON metadata in a trusted, exclusively owned
// private COPIED storage root. It never opens Lance tables, original author roots,
// infers registry state, repairs files, or establishes table/copy crosslinks.
// Caller must exclude concurrent writers and retain an independent physical
// source/copy proof. Entry identity checks detect ordinary changes, not malicious
// same-user restore-to-identical races. Returned errors/summary never include text,
// document IDs, filenames, configured paths, fingerprints or raw JSON.
package knowledgecopy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

var (
	ErrInput             = errors.New("KNOWLEDGECOPY_INPUT_INVALID")
	ErrUnsafe            = errors.New("KNOWLEDGECOPY_FILESYSTEM_UNSAFE")
	ErrChanged           = errors.New("KNOWLEDGECOPY_METADATA_CHANGED")
	ErrBudget            = errors.New("KNOWLEDGECOPY_BUDGET_EXCEEDED")
	ErrInvalid           = errors.New("KNOWLEDGECOPY_METADATA_INVALID")
	ErrLegacyUnsupported = errors.New("KNOWLEDGECOPY_LEGACY_UNSUPPORTED")
	ErrUnsupported       = errors.New("KNOWLEDGECOPY_PLATFORM_UNSUPPORTED")
)

const (
	MaxMetadataBytes int64 = 64 * 1024 * 1024
	MaxCopies              = 100000
)

// Budget bounds aggregate raw JSON bytes and number of copy files. Both limits
// must be positive and no greater than the exported maximum constants. Bytes
// bound input, not an exact peak-RSS guarantee for JSON parsing.
type Budget struct {
	MaxMetadataBytes int64
	MaxCopies        int
}

// Summary records metadata only. Current = !indexDirty && SHA256(content)==indexedHash;
// Edited is independent. Dirty and HashMismatch may overlap. Digests commit exact
// raw JSON bytes (including unknown fields/formatting), not normalized values.
// MetadataSHA256 includes optional-asset presence; CopiesSHA256 is a length-framed,
// filename-byte-sorted aggregate. No canonical table integrity is implied.
type Summary struct {
	RegistryPresent bool
	CopiesPresent   bool
	Spaces          int
	Active          int
	Inactive        int
	Building        int
	Copies          int
	Current         int
	Dirty           int
	Edited          int
	HashMismatch    int
	MetadataBytes   int64
	RegistrySHA256  string
	CopiesSHA256    string
	MetadataSHA256  string
}
type observed struct {
	name string
	info fs.FileInfo
}
type state struct {
	ctx    context.Context
	root   *os.Root
	budget Budget
	used   int64
	files  []observed
}

func ValidateBudget(budget Budget) error {
	if budget.MaxMetadataBytes <= 0 || budget.MaxMetadataBytes > MaxMetadataBytes || budget.MaxCopies <= 0 || budget.MaxCopies > MaxCopies {
		return ErrInput
	}
	return nil
}
func Validate(ctx context.Context, root *os.Root, budget Budget) (Summary, error) {
	if ctx == nil || root == nil {
		return Summary{}, ErrInput
	}
	if err := ValidateBudget(budget); err != nil {
		return Summary{}, err
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if !supported() {
		return Summary{}, ErrUnsupported
	}
	anchor, err := root.Stat(".")
	if err != nil || !privateDirectory(anchor) {
		return Summary{}, ErrUnsafe
	}
	s := state{ctx: ctx, root: root, budget: budget}
	out, err := s.validate()
	if err != nil {
		return Summary{}, err
	}
	after, err := root.Stat(".")
	if err != nil || !sameEntry(anchor, after) {
		return Summary{}, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	return out, nil
}
func legacy(ctx context.Context, root *os.Root) error {
	for _, name := range []string{"vectors.json", "vectors.json.migration-journal.json"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := root.Lstat(name)
		if err == nil {
			return ErrLegacyUnsupported
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return ErrUnsafe
		}
	}
	return nil
}
func optional(root *os.Root, name string) (fs.FileInfo, error) {
	v, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsafe
	}
	return v, nil
}
func verify(root *os.Root, name string, before fs.FileInfo) error {
	after, err := optional(root, name)
	if err != nil {
		return err
	}
	if before == nil && after == nil {
		return nil
	}
	if before == nil || after == nil || !sameEntry(before, after) {
		return ErrChanged
	}
	return nil
}
func frame(h hash.Hash, name string, data []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(name)))
	h.Write(size[:])
	h.Write([]byte(name))
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	h.Write(size[:])
	h.Write(data)
}
func sum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
func (s *state) read(root *os.Root, name string, before fs.FileInfo) ([]byte, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if !regular(before) {
		return nil, ErrUnsafe
	}
	remaining := s.budget.MaxMetadataBytes - s.used
	if before.Size() < 0 || before.Size() > remaining {
		return nil, ErrBudget
	}
	file, err := openRegular(root, name)
	if err != nil {
		return nil, ErrUnsafe
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !regular(opened) || !sameEntry(before, opened) {
		return nil, ErrChanged
	}
	raw := make([]byte, 0, int(before.Size()))
	buffer := make([]byte, 32768)
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		limit := int64(len(buffer))
		if remaining-int64(len(raw))+1 < limit {
			limit = remaining - int64(len(raw)) + 1
		}
		n, e := file.Read(buffer[:int(limit)])
		if n > 0 {
			raw = append(raw, buffer[:n]...)
			if int64(len(raw)) > remaining {
				return nil, ErrBudget
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil || n == 0 {
			return nil, ErrUnsafe
		}
	}
	final, err := file.Stat()
	if err != nil || !sameEntry(before, final) {
		return nil, ErrChanged
	}
	if err := verify(root, name, before); err != nil {
		return nil, err
	}
	if int64(len(raw)) != before.Size() {
		return nil, ErrChanged
	}
	s.used += int64(len(raw))
	return raw, nil
}
func (s *state) validate() (Summary, error) {
	var out Summary
	if err := legacy(s.ctx, s.root); err != nil {
		return out, err
	}
	registry, err := optional(s.root, "embedding-spaces.json")
	if err != nil {
		return out, err
	}
	copies, err := optional(s.root, "knowledge-copies")
	if err != nil {
		return out, err
	}
	all := sha256.New()
	copyhash := sha256.New()
	frame(all, "knowledgecopy-v1", nil)
	if registry != nil {
		raw, err := s.read(s.root, "embedding-spaces.json", registry)
		if err != nil {
			return out, err
		}
		value, err := parse(s.ctx, raw)
		if err != nil {
			return out, err
		}
		if err := registrySummary(s.ctx, value, &out); err != nil {
			return out, err
		}
		digest := sha256.Sum256(raw)
		out.RegistrySHA256 = hex.EncodeToString(digest[:])
		out.RegistryPresent = true
		frame(all, "embedding-spaces.json", raw)
	} else {
		frame(all, "registry-absent", nil)
	}
	if copies != nil {
		if !privateDirectory(copies) {
			return out, ErrUnsafe
		}
		child, err := s.root.OpenRoot("knowledge-copies")
		if err != nil {
			return out, ErrUnsafe
		}
		defer child.Close()
		pinned, err := child.Stat(".")
		if err != nil || !sameEntry(copies, pinned) {
			return out, ErrChanged
		}
		if err := verify(s.root, "knowledge-copies", copies); err != nil {
			return out, err
		}
		listing, err := openDirectory(child)
		if err != nil {
			return out, ErrUnsafe
		}
		defer listing.Close()
		var names []string
		for {
			if err := s.ctx.Err(); err != nil {
				return out, err
			}
			batch, e := listing.ReadDir(128)
			for _, entry := range batch {
				if len(names) >= s.budget.MaxCopies {
					return out, ErrBudget
				}
				name := entry.Name()
				if !copyName(name) {
					return out, ErrInvalid
				}
				names = append(names, name)
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				return out, ErrUnsafe
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if err := s.ctx.Err(); err != nil {
				return out, err
			}
			before, err := child.Lstat(name)
			if err != nil {
				return out, ErrChanged
			}
			raw, err := s.read(child, name, before)
			if err != nil {
				return out, err
			}
			value, err := parse(s.ctx, raw)
			if err != nil {
				return out, err
			}
			if err := copySummary(value, &out); err != nil {
				return out, err
			}
			frame(copyhash, name, raw)
			s.files = append(s.files, observed{name, before})
		}
		for _, file := range s.files {
			if err := s.ctx.Err(); err != nil {
				return out, err
			}
			if err := verify(child, file.name, file.info); err != nil {
				return out, err
			}
		}
		final, err := child.Stat(".")
		if err != nil || !sameEntry(pinned, final) {
			return out, ErrChanged
		}
		out.CopiesPresent = true
		frame(all, "copies-present", nil)
	} else {
		frame(all, "copies-absent", nil)
	}
	out.CopiesSHA256 = sum(copyhash)
	frame(all, "copies-digest", copyhash.Sum(nil))
	out.MetadataSHA256 = sum(all)
	out.MetadataBytes = s.used
	if err := legacy(s.ctx, s.root); err != nil {
		return Summary{}, err
	}
	if err := verify(s.root, "embedding-spaces.json", registry); err != nil {
		return Summary{}, err
	}
	if err := verify(s.root, "knowledge-copies", copies); err != nil {
		return Summary{}, err
	}
	return out, nil
}
func copyName(name string) bool {
	if len(name) != 69 || !strings.HasSuffix(name, ".json") {
		return false
	}
	for _, b := range []byte(name[:64]) {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}
