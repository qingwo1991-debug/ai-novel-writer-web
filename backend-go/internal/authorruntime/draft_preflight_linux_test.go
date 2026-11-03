//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestPreflightDraftCASReadTxRejectsStaleReceiptAndAliases(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"CREATE TABLE contents(id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE TABLE drafts(id INTEGER PRIMARY KEY, status TEXT, content_id INTEGER)",
		"CREATE TABLE reviews(id INTEGER PRIMARY KEY, content_id INTEGER)",
		"CREATE TABLE revisions(id INTEGER PRIMARY KEY, content_id INTEGER)",
		"CREATE TABLE finalization_outbox(draft_id INTEGER)",
		"INSERT INTO contents(id, body) VALUES(1, 'old')",
		"INSERT INTO drafts(id, status, content_id) VALUES(1, 'draft', 1)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	check := func(baseline string) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return preflightDraftCASReadTx(ctx, tx, 1, baseline)
	}
	if err := check("old"); err != nil {
		t.Fatalf("current working draft rejected: %v", err)
	}
	if err := check("stale"); !errors.Is(err, store.ErrDraftContentConflict) {
		t.Fatalf("stale body: %v", err)
	}
	for _, mutation := range []struct {
		add    string
		remove string
	}{
		{"INSERT INTO finalization_outbox(draft_id) VALUES(1)", "DELETE FROM finalization_outbox"},
		{"INSERT INTO drafts(id,status,content_id) VALUES(2,'draft',1)", "DELETE FROM drafts WHERE id=2"},
		{"INSERT INTO reviews(id,content_id) VALUES(1,1)", "DELETE FROM reviews"},
		{"INSERT INTO revisions(id,content_id) VALUES(1,1)", "DELETE FROM revisions"},
	} {
		if _, err := db.Exec(mutation.add); err != nil {
			t.Fatal(err)
		}
		if err := check("old"); !errors.Is(err, store.ErrDraftContentConflict) {
			t.Fatalf("authoritative receipt or alias accepted: %v", err)
		}
		if _, err := db.Exec(mutation.remove); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE drafts SET status='finalized' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := check("old"); !errors.Is(err, store.ErrDraftContentConflict) {
		t.Fatalf("finalized draft accepted: %v", err)
	}
}

func TestPreflightDraftCASRejectsMalformedInputBeforeLeaseRead(t *testing.T) {
	l := &ReadLifecycle{}
	baseline := "old"
	for _, request := range []struct {
		id       int64
		content  string
		count    int64
		baseline *string
	}{
		{0, "new", 3, &baseline},
		{1, "new", -1, &baseline},
		{1, "new", 3, nil},
		{1, "\xff", 3, &baseline},
		{1, strings.Repeat("x", maxPreflightDraftBytes+1), 3, &baseline},
	} {
		if err := l.PreflightDraftCAS(context.Background(),
			projectsession.Owner{}, projectsession.Context{}, request.id, request.content, request.count, request.baseline); !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed request entered lease read: %v", err)
		}
	}
}

func TestPreflightDraftCASReadTxSkipsOnlyConfirmedMissingReportTables(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"CREATE TABLE contents(id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE TABLE drafts(id INTEGER PRIMARY KEY, status TEXT, content_id INTEGER)",
		"CREATE TABLE finalization_outbox(draft_id INTEGER)",
		"INSERT INTO contents(id, body) VALUES(1, 'old')",
		"INSERT INTO drafts(id, status, content_id) VALUES(1, 'draft', 1)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	check := func() error {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return preflightDraftCASReadTx(ctx, tx, 1, "old")
	}
	if err := check(); err != nil {
		t.Fatalf("confirmed missing report tables rejected: %v", err)
	}
	for _, table := range []string{"reviews", "revisions"} {
		if _, err := db.Exec("CREATE TABLE " + table + "(id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
		if err := check(); !errors.Is(err, ErrDraftCASAuthorityIncomplete) {
			t.Fatalf("present malformed %s table accepted: %v", table, err)
		}
		if _, err := db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
}
