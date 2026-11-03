//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestReadLifecycleReviewRevisionListsKeepLeaseAndSource(t *testing.T) {
	catalog, key, config, sourcePath, _ := readFixture(t)
	seed, err := sql.Open("sqlite3", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := "编辑原稿\n🙂"
	result, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", content)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	result, err = seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(?,1,'draft',?)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	repository := store.NewReviewRevisions(seed)
	expectedSource := map[string]any{"id": draftID, "chapterNumber": chapter, "version": int64(1), "status": "draft", "content": content}
	review, err := repository.CreateReview(ctx, map[string]any{"baseDraftId": draftID, "content": "审稿意见", "expectedSource": expectedSource})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := repository.CreateRevision(ctx, map[string]any{"baseDraftId": draftID, "content": "修稿建议", "revisionType": "refine", "wordCount": int64(4), "expectedSource": expectedSource})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "review-cookie", ClientID: "review-tab"}
	identity, err := lifecycle.Open(ctx, owner, key, "review-open")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		operation string
		wantID    int64
	}{
		{"db:review-list", review["id"].(int64)},
		{"db:revision-get-pending", revision["id"].(int64)},
		{"db:revision-list", revision["id"].(int64)},
	} {
		wire, err := lifecycle.ReadReviewRevisionJSON(ctx, owner, identity, tc.operation, draftID)
		if err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		if err := json.Unmarshal(wire, &rows); err != nil || len(rows) != 1 || rows[0]["id"] != float64(tc.wantID) || rows[0]["baseDraftId"] != float64(draftID) {
			t.Fatalf("%s = %s, %v", tc.operation, wire, err)
		}
	}
	if _, err := lifecycle.ReadReviewRevisionJSON(ctx, projectsession.Owner{SessionID: owner.SessionID, ClientID: "foreign-tab"}, identity, "db:review-list", draftID); err == nil {
		t.Fatal("cross-tab review read accepted")
	}
	stale := identity
	stale.LeaseID = "old-lease"
	if _, err := lifecycle.ReadReviewRevisionJSON(ctx, owner, stale, "db:revision-get-pending", draftID); err == nil {
		t.Fatal("stale revision lease accepted")
	}
	if _, err := lifecycle.ReadReviewRevisionJSON(ctx, owner, identity, "db:revision-merge", draftID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revision mutation accepted: %v", err)
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("review reads changed author bytes: %v", err)
	}
}
