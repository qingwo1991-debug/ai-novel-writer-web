//go:build linux

package authorruntime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// CommitDraftCASDurable commits one full-body draft CAS directly on the admitted
// author database under the frozen cookie/tab/lease fence. Unlike the read-only
// DraftCommitService it never stages a private copy or switches the source
// tree: the durable lane already holds the real writable project.db and the
// legacy writer lock is exclusive, so store.Drafts.UpdateContentIfCurrent runs
// the authoritative CAS (BEGIN IMMEDIATE, baseline compare, receipt and content
// ownership fences, then both row updates) on the author's own connection. A
// stale or replayed expected revision fails with store.ErrDraftContentConflict
// exactly like the read-only lane's preflight. On success the permit adopts the
// new committed content identity and the frozen lease rotates in place: the
// same admittedAuthor handle stays installed, so the durable write session is
// never downgraded to a read-only handle.
func (l *ReadLifecycle) CommitDraftCASDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, draftID int64, content string, wordCount int64, expectedContent *string) (projectsession.Context, error) {
	var zero projectsession.Context
	if l == nil || l.active == nil || ctx == nil || draftID <= 0 || draftID > maxPreflightSafeInteger ||
		wordCount < 0 || wordCount > maxPreflightSafeInteger || expectedContent == nil ||
		!utf8.ValidString(content) || !utf8.ValidString(*expectedContent) ||
		len(content) > maxPreflightDraftBytes || len(*expectedContent) > maxPreflightDraftBytes {
		return zero, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return zero, err
	}
	defer done()
	author, ok := handle.(*admittedAuthor)
	if !ok {
		return zero, ErrDurableInvalid
	}
	author.mu.Lock()
	defer author.mu.Unlock()
	if author.closed || author.handle == nil || author.permit == nil {
		return zero, ErrDurableClosed
	}
	if err := author.permit.Verify(ctx, author.project); err != nil {
		return zero, err
	}
	if err := author.handle.Verify(ctx); err != nil {
		return zero, err
	}
	if err := store.NewDrafts(author.handle.DB()).UpdateContentIfCurrent(ctx, draftID, content, wordCount, expectedContent); err != nil {
		return zero, err
	}
	// The commit changed the database content, so adopt the new committed
	// content identity before re-proving the handle, otherwise the post-write
	// verify would treat our own write as a stale replacement (mirrors SaveCore).
	if err := author.permit.Refresh(ctx); err != nil {
		return zero, err
	}
	if err := author.handle.Verify(ctx); err != nil {
		return zero, err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return zero, err
	}
	author.context.LeaseID = base64.RawURLEncoding.EncodeToString(nonce[:])
	return author.context, nil
}
