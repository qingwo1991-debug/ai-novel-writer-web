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
)

func TestReadLifecycleDraftReadsKeepExactLeaseAndSource(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	contentResult, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "保留正文\n🙂")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := contentResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	draftResult, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(?,1,'draft',?)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := draftResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "draft-cookie", ClientID: "draft-tab"}
	identity, err := lifecycle.Open(context.Background(), owner, key, "draft-open")
	if err != nil {
		t.Fatal(err)
	}
	full, err := lifecycle.ReadDraftJSON(context.Background(), owner, identity, "db:draft-get-full", draftID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		ID      int64  `json:"id"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(full, &parsed); err != nil || parsed.ID != draftID || parsed.Content != "保留正文\n🙂" {
		t.Fatalf("full draft = %s, %v", full, err)
	}
	list, err := lifecycle.ReadDraftJSON(context.Background(), owner, identity, "db:draft-list", chapter)
	if err != nil || !json.Valid(list) || len(list) == 0 || list[0] != '[' {
		t.Fatalf("draft list = %s, %v", list, err)
	}
	if _, err := lifecycle.ReadDraftJSON(context.Background(), projectsession.Owner{SessionID: owner.SessionID, ClientID: "foreign-tab"}, identity, "db:draft-get-full", draftID); err == nil {
		t.Fatal("cross-tab draft read accepted")
	}
	stale := identity
	stale.LeaseID = "stale-lease"
	if _, err := lifecycle.ReadDraftJSON(context.Background(), owner, stale, "db:draft-get-full", draftID); err == nil {
		t.Fatal("stale draft lease accepted")
	}
	if _, err := lifecycle.ReadDraftJSON(context.Background(), owner, identity, "db:draft-update-content", draftID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("draft write accepted: %v", err)
	}
	if err := lifecycle.Close(context.Background(), owner, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ReadDraftJSON(context.Background(), owner, identity, "db:draft-get-full", draftID); err == nil {
		t.Fatal("closed lease accepted")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("draft reads changed source bytes: %v", err)
	}
}
