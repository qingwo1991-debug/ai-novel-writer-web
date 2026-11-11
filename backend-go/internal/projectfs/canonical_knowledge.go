package projectfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

// CanonicalKnowledgeName is the canonical knowledge snapshot this lane's own
// knowledge operations write and every served kb read decodes: one bounded JSON
// document holding the store's `documents` and `chunks` tables. The shipped
// desktop knowledge base has no such file - it keeps its rows in LanceDB beside a
// `knowledge-copies/` directory - so the snapshot's presence and decodability is
// what separates knowledge this lane wrote and can manage from an asset that has
// no Go reader and no Go writer at all.
const CanonicalKnowledgeName = "knowledge-snapshot.json"

// The reference's copy layout (electron/vector-store.ts:130-174): copies live in
// knowledge-copies/, and a document row is managed exactly when its filePath
// starts with the knowledge-copy: marker. The same values are the served writer's
// productionKnowledgeCopyDirectory/Marker/Suffix.
const (
	canonicalKnowledgeCopyDirectory = "knowledge-copies"
	canonicalKnowledgeCopyMarker    = "knowledge-copy:"
	canonicalKnowledgeCopySuffix    = ".json"
	// canonicalKnowledgeDocumentsTable is the one table whose rows name the
	// documents a copy file can belong to.
	canonicalKnowledgeDocumentsTable = "documents"
	// canonicalKnowledgeMaxCopies is internal/knowledgecopy's own copy ceiling
	// (MaxCopies), so this listing is bounded exactly like the metadata capture.
	canonicalKnowledgeMaxCopies = 100000
)

// errCanonicalKnowledgeUnproved is a decision, not a failure: a snapshot this
// lane cannot decode is knowledge it cannot claim. It is never returned to an
// admission caller, which keeps its own refusal code.
var errCanonicalKnowledgeUnproved = errors.New("PROJECT_CANONICAL_KNOWLEDGE_UNPROVED")

// CanonicalKnowledge is the proof that .ai-novel holds this lane's own knowledge
// store. It carries decode and membership outcomes only: never a document id, a
// filename, a path, a digest or any author text.
type CanonicalKnowledge struct {
	// Snapshot is true only when knowledge-snapshot.json is a present, bounded,
	// single-link regular file that decodes through the same domain validator the
	// served knowledge reads run. A present but undecodable snapshot is false.
	Snapshot bool `json:"snapshot"`
	// CopiesMatch is true when every file in knowledge-copies/ is named after a
	// document row of that decoded snapshot (sha256hex(docId)+".json"), i.e. the
	// reference-format copies describe no document the canonical snapshot does
	// not. A copy a snapshot document does not name is not adopted: the store is
	// then not solely this lane's and stays unproved. A MISSING copy is not a
	// mismatch, because this lane's own reads report such a document as
	// unavailable rather than invalid, exactly as the reference does.
	CopiesMatch bool `json:"copiesMatch"`
}

// Proved reports whether the project's knowledge is provably this lane's own.
func (k CanonicalKnowledge) Proved() bool { return k.Snapshot && k.CopiesMatch }

// ProveCanonicalKnowledge reports whether the project's knowledge assets are the
// store this lane's own knowledge operations write. It reads only the two fixed
// entries above, through the same confined, no-follow, bounded primitives the
// inventory uses, and returns no content.
//
// It never errors on unproved knowledge. An absent, unsafe (symlink, hardlink or
// special file), oversized or mid-change snapshot, and a snapshot that does not
// decode, all return a zero value with a nil error: the caller's admission keeps
// its own registered refusal instead of acquiring a second, weaker failure mode.
// Only cancellation, a closed project and a project whose identity no longer
// matches are errors.
func (p *Project) ProveCanonicalKnowledge(ctx context.Context) (CanonicalKnowledge, error) {
	var proof CanonicalKnowledge
	if ctx == nil {
		return proof, ErrInvalidPath
	}
	if err := ctx.Err(); err != nil {
		return proof, err
	}
	if p == nil || p.library == nil {
		return proof, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.library.mu.Lock()
	defer p.library.mu.Unlock()
	if err := p.checkIdentityLocked(); err != nil {
		return proof, portableAuthorityIdentityError(err)
	}
	raw, err := readBounded(p.storage, CanonicalKnowledgeName, int64(knowledgedomain.HardMaxTextBytes))
	if err != nil {
		return CanonicalKnowledge{}, nil
	}
	documents, decodeErr := canonicalKnowledgeDocuments(ctx, raw)
	if decodeErr != nil {
		return CanonicalKnowledge{}, nil
	}
	proof.Snapshot = true
	matched, copyErr := p.canonicalKnowledgeCopiesMatch(ctx, documents)
	if copyErr != nil {
		if contextError := ctx.Err(); contextError != nil {
			return CanonicalKnowledge{}, contextError
		}
		return CanonicalKnowledge{}, nil
	}
	proof.CopiesMatch = matched
	if err := p.checkIdentityLocked(); err != nil {
		return CanonicalKnowledge{}, portableAuthorityIdentityError(err)
	}
	if err := ctx.Err(); err != nil {
		return CanonicalKnowledge{}, err
	}
	return proof, nil
}

// canonicalKnowledgeDocuments decodes the snapshot through the same domain
// validator the served knowledge reads run (with the same UseNumber decoding and
// the same trailing-data refusal), then returns the ids of the document rows
// whose filePath marks them as copy-managed. Any other outcome is unproved.
func canonicalKnowledgeDocuments(ctx context.Context, raw []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var snapshot knowledgedomain.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, errCanonicalKnowledgeUnproved
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errCanonicalKnowledgeUnproved
	}
	if _, err := knowledgedomain.Validate(ctx, snapshot, canonicalKnowledgeBudget()); err != nil {
		return nil, errCanonicalKnowledgeUnproved
	}
	documents := make([]string, 0, 8)
	for _, table := range snapshot.Tables {
		if table.Name != canonicalKnowledgeDocumentsTable {
			continue
		}
		for _, row := range table.Records {
			id, idOK := row["id"].(string)
			filePath, pathOK := row["filePath"].(string)
			if !idOK || id == "" || !pathOK {
				return nil, errCanonicalKnowledgeUnproved
			}
			if !strings.HasPrefix(filePath, canonicalKnowledgeCopyMarker) {
				continue
			}
			documents = append(documents, id)
		}
	}
	return documents, nil
}

// canonicalKnowledgeBudget is the served readers' own budget: the hard limits
// internal/knowledgedomain publishes.
func canonicalKnowledgeBudget() knowledgedomain.Budget {
	return knowledgedomain.Budget{
		MaxTables:    knowledgedomain.HardMaxTables,
		MaxRows:      knowledgedomain.HardMaxRows,
		MaxCells:     knowledgedomain.HardMaxCells,
		MaxTextBytes: knowledgedomain.HardMaxTextBytes,
	}
}

// canonicalKnowledgeCopyName is the reference's copy filename for one document
// id: lowercase sha256hex of the exact UTF-8 id plus ".json"
// (vector-store.ts:135-142, isDocumentCopyIndexCurrent).
func canonicalKnowledgeCopyName(documentID string) string {
	digest := sha256.Sum256([]byte(documentID))
	return hex.EncodeToString(digest[:]) + canonicalKnowledgeCopySuffix
}

// canonicalKnowledgeCopiesMatch judges the copy tree against the decoded
// snapshot's managed documents. Every observed copy must be a single-link regular
// file named after one of them; a snapshot document without a copy is tolerated.
func (p *Project) canonicalKnowledgeCopiesMatch(ctx context.Context, documents []string) (bool, error) {
	info, err := p.storage.Lstat(canonicalKnowledgeCopyDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		// No copies at all: nothing can contradict the snapshot.
		return true, nil
	}
	if err != nil || !directory(info) {
		return false, ErrUnsafeDirectory
	}
	child, _, err := openChild(p.storage, canonicalKnowledgeCopyDirectory)
	if err != nil {
		return false, err
	}
	defer child.Close()
	names, err := knowledgeNames(ctx, child, canonicalKnowledgeMaxCopies)
	if err != nil {
		return false, err
	}
	expected := make(map[string]bool, len(documents))
	for _, documentID := range documents {
		expected[canonicalKnowledgeCopyName(documentID)] = true
	}
	for _, name := range names {
		if !expected[name] {
			// A copy no canonical document names is knowledge this snapshot does
			// not describe. Adopting it would attach the lane to data it cannot
			// account for, so the store stays unproved instead.
			return false, nil
		}
		copyInfo, err := child.Lstat(name)
		if err != nil || !copyInfo.Mode().IsRegular() || !singleLink(copyInfo) {
			return false, ErrUnsafeFile
		}
	}
	return true, nil
}
