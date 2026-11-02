package knowledgecopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"unicode/utf8"
)

// DocumentRef is a document identity and its original filePath marker. Callers
// supply admitted document rows, not chunk IDs or paths to original author files.
// Neither field is interpreted as a filesystem path or returned in diagnostics.
type DocumentRef struct {
	ID       string
	FilePath string
}

// LinkSummary counts document/copy links, not table integrity. Documents includes
// unmanaged rows; all other document-state counts concern managed rows only.
// Managed = Present + Missing. Missing is nonfatal and never current. For present
// copies Current = !indexDirty && SHA256(content) == indexedHash; Edited does not
// affect Current, and Dirty/Edited/HashMismatch may overlap. Unreferenced counts
// validated copy files whose names are not derived from a managed document ID.
// No content, IDs, filenames, paths or fingerprints are exposed.
type LinkSummary struct {
	Documents    int `json:"documents"`
	Managed      int `json:"managed"`
	Present      int `json:"present"`
	Missing      int `json:"missing"`
	Current      int `json:"current"`
	Dirty        int `json:"dirty"`
	Edited       int `json:"edited"`
	HashMismatch int `json:"hashMismatch"`
	Unreferenced int `json:"unreferenced"`
}

// ValidateLinks independently validates all private copied metadata, then
// classifies admitted document references without changing any file. Its root,
// physical source/copy proof, platform and quiescence requirements are the same
// as Validate: the caller must own a trusted private COPIED root, independently
// establish physical staging, and exclude concurrent document/metadata writers.
// It does not open Lance tables, compare content with chunk text, repair orphans,
// establish registry/table relations, or grant source admission merely on success.
// Callers requiring current sources must separately inspect the relevant counts.
//
// IDs must be nonempty, unique, valid UTF-8; FilePath must be valid UTF-8. Invalid
// references yield ErrInput. Budget.MaxCopies independently bounds both document
// references (including unmanaged ones) and all copy files. Aggregate raw UTF-8
// bytes of IDs and FilePaths are separately bounded by MaxMetadataBytes before
// UTF-8 validation or hashing. Metadata JSON bytes across the registry and every
// copy file are charged once against the budget, independently of reference bytes.
// Implementation uses two bounded physical passes: one complete metadata pass,
// then only referenced copies again (at most MaxMetadataBytes each pass), retaining
// and rechecking first-pass identities. It does not charge the reread twice.
//
// Compatibility follows electron/vector-store.ts isDocumentCopyIndexCurrent:
// only the case-sensitive "knowledge-copy:" prefix makes a row managed; the
// suffix is ignored, and lookup uses lowercase SHA256 of the exact UTF-8 ID plus
// ".json". Unmanaged rows require no file; the original predicate returns true
// for them, but they are deliberately excluded from this summary's Current count.
// All copy files, including unreferenced ones, must satisfy Validate's metadata
// and filesystem guards. Missing managed files are classified, not rejected;
// dirty, edited and hash-mismatched copies are likewise valid metadata states.
// Every failure returns a zero summary and only symbolic package/context errors.
func ValidateLinks(ctx context.Context, root *os.Root, docs []DocumentRef, budget Budget) (LinkSummary, error) {
	if ctx == nil || root == nil {
		return LinkSummary{}, ErrInput
	}
	if err := ValidateBudget(budget); err != nil {
		return LinkSummary{}, err
	}
	if err := ctx.Err(); err != nil {
		return LinkSummary{}, err
	}
	if !supported() {
		return LinkSummary{}, ErrUnsupported
	}
	if len(docs) > budget.MaxCopies {
		return LinkSummary{}, ErrBudget
	}
	var referenceBytes int64
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return LinkSummary{}, err
		}
		if int64(len(doc.ID)) > budget.MaxMetadataBytes-referenceBytes {
			return LinkSummary{}, ErrBudget
		}
		referenceBytes += int64(len(doc.ID))
		if int64(len(doc.FilePath)) > budget.MaxMetadataBytes-referenceBytes {
			return LinkSummary{}, ErrBudget
		}
		referenceBytes += int64(len(doc.FilePath))
	}
	seen := make(map[string]bool, len(docs))
	managed := make(map[string]int)
	var out LinkSummary
	out.Documents = len(docs)
	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return LinkSummary{}, err
		}
		if doc.ID == "" || !utf8.ValidString(doc.ID) || !utf8.ValidString(doc.FilePath) || seen[doc.ID] {
			return LinkSummary{}, ErrInput
		}
		seen[doc.ID] = true
		if strings.HasPrefix(doc.FilePath, "knowledge-copy:") {
			digest := sha256.Sum256([]byte(doc.ID))
			managed[hex.EncodeToString(digest[:])+".json"]++
			out.Managed++
		}
	}
	result, err := validateLinks(ctx, root, budget, managed, out)
	if err != nil {
		return LinkSummary{}, err
	}
	return result, nil
}

func validateLinks(ctx context.Context, root *os.Root, budget Budget, managed map[string]int, out LinkSummary) (LinkSummary, error) {
	anchor, err := root.Stat(".")
	if err != nil || !privateDirectory(anchor) {
		return LinkSummary{}, ErrUnsafe
	}
	registry, err := optional(root, "embedding-spaces.json")
	if err != nil {
		return LinkSummary{}, err
	}
	copies, err := optional(root, "knowledge-copies")
	if err != nil {
		return LinkSummary{}, err
	}
	first := state{ctx: ctx, root: root, budget: budget}
	metadata, err := first.validate()
	if err != nil {
		return LinkSummary{}, err
	}
	if err := verify(root, "embedding-spaces.json", registry); err != nil {
		return LinkSummary{}, err
	}
	if err := verify(root, "knowledge-copies", copies); err != nil {
		return LinkSummary{}, err
	}
	out.Unreferenced = metadata.Copies
	if copies != nil {
		if !privateDirectory(copies) {
			return LinkSummary{}, ErrUnsafe
		}
		child, err := root.OpenRoot("knowledge-copies")
		if err != nil {
			return LinkSummary{}, ErrUnsafe
		}
		defer child.Close()
		pinned, err := child.Stat(".")
		if err != nil || !sameEntry(copies, pinned) {
			return LinkSummary{}, ErrChanged
		}
		second := state{ctx: ctx, root: root, budget: budget}
		for _, file := range first.files {
			if err := ctx.Err(); err != nil {
				return LinkSummary{}, err
			}
			count := managed[file.name]
			if count == 0 {
				continue
			}
			raw, err := second.read(child, file.name, file.info)
			if err != nil {
				return LinkSummary{}, err
			}
			value, err := parse(ctx, raw)
			if err != nil {
				return LinkSummary{}, err
			}
			var classified Summary
			if err := copySummary(value, &classified); err != nil {
				return LinkSummary{}, err
			}
			out.Present += count
			out.Current += classified.Current * count
			out.Dirty += classified.Dirty * count
			out.Edited += classified.Edited * count
			out.HashMismatch += classified.HashMismatch * count
			out.Unreferenced--
		}
		for _, file := range first.files {
			if err := ctx.Err(); err != nil {
				return LinkSummary{}, err
			}
			if err := verify(child, file.name, file.info); err != nil {
				return LinkSummary{}, err
			}
		}
		after, err := child.Stat(".")
		if err != nil || !sameEntry(pinned, after) {
			return LinkSummary{}, ErrChanged
		}
	}
	out.Missing = out.Managed - out.Present
	if err := legacy(ctx, root); err != nil {
		return LinkSummary{}, err
	}
	if err := verify(root, "embedding-spaces.json", registry); err != nil {
		return LinkSummary{}, err
	}
	if err := verify(root, "knowledge-copies", copies); err != nil {
		return LinkSummary{}, err
	}
	after, err := root.Stat(".")
	if err != nil || !sameEntry(anchor, after) {
		return LinkSummary{}, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return LinkSummary{}, err
	}
	return out, nil
}
