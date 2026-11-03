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

func TestReadLifecycleFullReviewRevisionKeepsLeaseAndSource(t *testing.T) {
	catalog, key, config, sourcePath, _ := readFixture(t)
	seed, err := sql.Open("sqlite3", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := "编辑原稿\n🙂"
	inserted, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", content)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	inserted, err = seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(?,1,'draft',?)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
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
	owner := projectsession.Owner{SessionID: "full-cookie", ClientID: "full-tab"}
	identity, err := lifecycle.Open(ctx, owner, key, "full-open")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		operation string
		id        int64
		want      string
	}{
		{"db:review-get-full", review["id"].(int64), "审稿意见"},
		{"db:revision-get-full", revision["id"].(int64), "修稿建议"},
	} {
		wire, err := lifecycle.ReadReviewRevisionFullJSON(ctx, owner, identity, tc.operation, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(wire, &row); err != nil || row["id"] != float64(tc.id) || row["content"] != tc.want || row["sourceDraft"].(map[string]any)["content"] != content {
			t.Fatalf("%s = %s, %v", tc.operation, wire, err)
		}
	}
	missing, err := lifecycle.ReadReviewRevisionFullJSON(ctx, owner, identity, "db:review-get-full", 999999)
	if err != nil || string(missing) != "null" {
		t.Fatalf("missing full review = %s, %v", missing, err)
	}
	if _, err := lifecycle.ReadReviewRevisionFullJSON(ctx, projectsession.Owner{SessionID: owner.SessionID, ClientID: "foreign"}, identity, "db:review-get-full", review["id"].(int64)); err == nil {
		t.Fatal("cross-tab full review accepted")
	}
	stale := identity
	stale.LeaseID = "old-lease"
	if _, err := lifecycle.ReadReviewRevisionFullJSON(ctx, owner, stale, "db:revision-get-full", revision["id"].(int64)); err == nil {
		t.Fatal("stale full revision lease accepted")
	}
	if _, err := lifecycle.ReadReviewRevisionFullJSON(ctx, owner, identity, "db:revision-merge", revision["id"].(int64)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revision mutation accepted: %v", err)
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("full reads changed author bytes: %v", err)
	}
}
