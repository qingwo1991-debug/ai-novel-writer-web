package knowledgecopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// Copy is immutable-by-value captured metadata. Content is intentional trusted
// application data, not a diagnostic. Unknown JSON properties remain on disk.
type Copy struct {
	Content     string
	IndexedHash string
	Edited      bool
	IndexDirty  bool
}

// ReadManagedCopies captures managed, present copies keyed by exact document ID.
// Missing and unmanaged references are omitted. No source root or descriptor is
// returned. Caller must retain independent complete physical source/copy proofs
// and continuous exclusion of writers: these identity observations do not defeat
// hostile same-UID replacement/restoration races.
//
// ValidateLinks first enforces all reference, metadata and platform budgets. A
// second metadata pass retains first-file identities for capture; only matched
// files are then reread, using the same guarded reader/parser. Each full or
// referenced pass is independently bounded by MaxMetadataBytes (at most four
// bounded passes overall). Captured content is aggregate bounded by that budget.
// No file is repaired, created, or deleted. Every error returns a nil map and
// symbolic package/context errors, never prose, paths or IDs.
func ReadManagedCopies(ctx context.Context, root *os.Root, refs []DocumentRef, budget Budget) (map[string]Copy, error) {
	if _, err := ValidateLinks(ctx, root, refs, budget); err != nil {
		return nil, err
	}
	anchor, err := root.Stat(".")
	if err != nil || !privateDirectory(anchor) {
		return nil, ErrUnsafe
	}
	registry, err := optional(root, "embedding-spaces.json")
	if err != nil {
		return nil, err
	}
	copies, err := optional(root, "knowledge-copies")
	if err != nil {
		return nil, err
	}
	first := state{ctx: ctx, root: root, budget: budget}
	if _, err := first.validate(); err != nil {
		return nil, err
	}
	if err := verify(root, "embedding-spaces.json", registry); err != nil {
		return nil, err
	}
	if err := verify(root, "knowledge-copies", copies); err != nil {
		return nil, err
	}
	out := make(map[string]Copy)
	managed := make(map[string][]string)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(ref.FilePath, "knowledge-copy:") {
			sum := sha256.Sum256([]byte(ref.ID))
			name := hex.EncodeToString(sum[:]) + ".json"
			managed[name] = append(managed[name], ref.ID)
		}
	}
	if copies != nil {
		if !privateDirectory(copies) {
			return nil, ErrUnsafe
		}
		child, err := root.OpenRoot("knowledge-copies")
		if err != nil {
			return nil, ErrUnsafe
		}
		defer child.Close()
		pinned, err := child.Stat(".")
		if err != nil || !sameEntry(copies, pinned) {
			return nil, ErrChanged
		}
		second := state{ctx: ctx, root: root, budget: budget}
		for _, file := range first.files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ids := managed[file.name]
			if len(ids) == 0 {
				continue
			}
			raw, err := second.read(child, file.name, file.info)
			if err != nil {
				return nil, err
			}
			value, err := parse(ctx, raw)
			if err != nil {
				return nil, err
			}
			var summary Summary
			if err := copySummary(value, &summary); err != nil {
				return nil, err
			}
			obj, ok := object(value)
			if !ok {
				return nil, ErrInvalid
			}
			copy := Copy{Content: obj["content"].(string), IndexedHash: obj["indexedHash"].(string), Edited: obj["edited"].(bool), IndexDirty: obj["indexDirty"].(bool)}
			for _, id := range ids {
				out[id] = copy
			}
		}
		for _, file := range first.files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := verify(child, file.name, file.info); err != nil {
				return nil, err
			}
		}
		after, err := child.Stat(".")
		if err != nil || !sameEntry(pinned, after) {
			return nil, ErrChanged
		}
	}
	if err := legacy(ctx, root); err != nil {
		return nil, err
	}
	if err := verify(root, "embedding-spaces.json", registry); err != nil {
		return nil, err
	}
	if err := verify(root, "knowledge-copies", copies); err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !sameEntry(anchor, after) {
		return nil, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
