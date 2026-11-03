package knowledgecopy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

var (
	ErrCopyUnavailable = errors.New("KNOWLEDGE_BASE_COPY_UNAVAILABLE")
	ErrCopyChanged     = errors.New("KNOWLEDGE_BASE_COPY_CHANGED")
)

// SaveManagedCopy performs the original save-document-copy content-hash CAS on a
// trusted private copied root. The caller must continuously exclude all other
// writers and independently prove the root belongs to the admitted project.
// In particular, a successful call here is not an author-project commit.
// refs are the complete admitted document list; a caller-supplied ID alone
// never grants authority over a file. Unknown copy properties are preserved.
func SaveManagedCopy(ctx context.Context, root *os.Root, refs []DocumentRef, budget Budget, id, content, expectedHash string) error {
	if ctx == nil || root == nil || id == "" || !utf8.ValidString(id) || !utf8.ValidString(content) || !utf8.ValidString(expectedHash) {
		return ErrInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := ValidateLinks(ctx, root, refs, budget); err != nil {
		return err
	}
	managed := false
	for _, ref := range refs {
		if ref.ID == id && strings.HasPrefix(ref.FilePath, "knowledge-copy:") {
			managed = true
			break
		}
	}
	if !managed {
		return ErrCopyUnavailable
	}
	before, err := Validate(ctx, root, budget)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(id))
	name := hex.EncodeToString(digest[:]) + ".json"
	copiesInfo, err := optional(root, "knowledge-copies")
	if err != nil {
		return err
	}
	if copiesInfo == nil {
		return ErrCopyUnavailable
	}
	if !privateDirectory(copiesInfo) {
		return ErrUnsafe
	}
	child, err := root.OpenRoot("knowledge-copies")
	if err != nil {
		return ErrUnsafe
	}
	defer child.Close()
	childInfo, err := child.Stat(".")
	if err != nil || !sameEntry(copiesInfo, childInfo) {
		return ErrChanged
	}
	fileInfo, err := optional(child, name)
	if err != nil {
		return err
	}
	if fileInfo == nil {
		return ErrCopyUnavailable
	}
	reader := state{ctx: ctx, root: child, budget: budget}
	raw, err := reader.read(child, name, fileInfo)
	if err != nil {
		return err
	}
	value, err := parse(ctx, raw)
	if err != nil {
		return err
	}
	if err := copySummary(value, &Summary{}); err != nil {
		return err
	}
	object, ok := object(value)
	if !ok {
		return ErrInvalid
	}
	current := sha256.Sum256([]byte(object["content"].(string)))
	if hex.EncodeToString(current[:]) != expectedHash {
		return ErrCopyChanged
	}
	object["content"] = content
	object["edited"] = true
	object["indexDirty"] = true
	next, err := json.Marshal(object)
	if err != nil {
		return ErrInvalid
	}
	if int64(len(next)) > budget.MaxMetadataBytes-before.MetadataBytes+int64(len(raw)) {
		return ErrBudget
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrUnsafe
	}
	temporary := name + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := createExclusive(child, temporary)
	if err != nil {
		return ErrUnsafe
	}
	defer child.Remove(temporary)
	writeErr := error(nil)
	if written, err := file.Write(next); err != nil {
		writeErr = err
	} else if written != len(next) {
		writeErr = ErrUnsafe
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := file.Close(); writeErr == nil {
		writeErr = err
	}
	if writeErr != nil {
		return ErrUnsafe
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verify(child, name, fileInfo); err != nil {
		return err
	}
	// Creating our temporary file legitimately changes directory timestamps.
	// Keep the pinned directory inode and private ownership, while retaining the
	// exact old file identity as the CAS target.
	currentDir, err := optional(root, "knowledge-copies")
	if err != nil || currentDir == nil || !os.SameFile(copiesInfo, currentDir) || !privateDirectory(currentDir) {
		return ErrChanged
	}
	if err := child.Rename(temporary, name); err != nil {
		return ErrUnsafe
	}
	// A failed directory sync leaves durability uncertain and must not be reported
	// as a successful author save by a future caller.
	directory, err := openDirectory(child)
	if err != nil {
		return ErrUnsafe
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return ErrUnsafe
	}
	return nil
}
