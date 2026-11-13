package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Test-only injected owner. No corresponding permissive verifier, proof minting
// or renderer-facing commit helper is shipped in production.
type foTestVerifier func(context.Context, *sql.Conn, FinalizationScope, FinalizationMutation, any) error

func (fn foTestVerifier) VerifyFinalizationOwnerProof(ctx context.Context, conn *sql.Conn, scope FinalizationScope, mutation FinalizationMutation, proof any) error {
	return fn(ctx, conn, scope, mutation, proof)
}

var foTestScope = FinalizationScope{ProjectID: "synthetic-project", Epoch: "synthetic-epoch"}

func foFixture(t *testing.T) (*sql.DB, *FinalizationOutbox, any) {
	t.Helper()
	db, _, _ := rrFixture(t) // Original baseline-v7 private clone; never fixture DDL.
	proof := new(int)
	owner := foTestVerifier(func(_ context.Context, _ *sql.Conn, scope FinalizationScope, _ FinalizationMutation, actual any) error {
		if scope != foTestScope || actual != proof {
			return ErrFinalizationOwnerRequired
		}
		return nil
	})
	return db, NewFinalizationOutbox(db, foTestScope, owner), proof
}
func foInput() FinalizationCommitInput {
	baseline := "原稿\r\n😀\x00é "
	body := "Final 中文𠮷 café cafe\u0301 l’amour 12 😀\n<&>\u2028\u2029"
	return FinalizationCommitInput{FinalizationID: "synthetic-finalization", DraftID: 900001,
		ChapterNumber: 9, ChapterTitle: "Title 中文 <&>\u2028", Content: body,
		ExpectedContent: &baseline, ContentHash: summaryHash(body), ContentRevision: 3, TargetFileName: "第9章 Title.txt"}
}
func foState(t *testing.T, db *sql.DB) any {
	t.Helper()
	out := map[string]any{}
	for _, table := range []string{"contents", "drafts", "finalization_outbox", "continuity_projection_meta", "character_identity_proposals"} {
		rows, err := rrMaps(context.Background(), db, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		out[table] = rows
	}
	return out
}

func TestFinalizationOutboxRequiresInjectedOwnerProofAndCapturedScope(t *testing.T) {
	db, repository, proof := foFixture(t)
	ctx := context.Background()
	for _, candidate := range []*FinalizationOutbox{
		NewFinalizationOutbox(db, foTestScope, nil),
		NewFinalizationOutbox(db, FinalizationScope{}, repository.owner),
		repository,
	} {
		before := foState(t, db)
		if _, err := candidate.CommitWithOwnerProof(ctx, foInput(), nil); !errors.Is(err, ErrFinalizationOwnerRequired) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, foState(t, db)) {
			t.Fatal("unauthorized commit wrote")
		}
	}
	if _, err := repository.CommitWithOwnerProof(ctx, foInput(), new(int)); !errors.Is(err, ErrFinalizationOwnerRequired) {
		t.Fatal(err)
	}
	calls := 0
	repository.owner = foTestVerifier(func(ctx context.Context, conn *sql.Conn, scope FinalizationScope, mutation FinalizationMutation, actual any) error {
		calls++
		if scope != foTestScope || actual != proof || mutation.Action != "commit" || mutation.Input.ContentHash != foInput().ContentHash || mutation.Input.ExpectedContent == nil || *mutation.Input.ExpectedContent != *foInput().ExpectedContent {
			t.Fatal("lost owner binding")
		}
		var body string
		if err := conn.QueryRowContext(ctx, "SELECT body FROM contents WHERE id=900001").Scan(&body); err != nil || body != *foInput().ExpectedContent {
			t.Fatal(body, err)
		}
		// A verifier cannot rewrite the operation's baseline through its copy.
		*mutation.Input.ExpectedContent = "malicious verifier mutation"
		return nil
	})
	if _, err := repository.CommitWithOwnerProof(ctx, foInput(), proof); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("owner not called exactly once", calls)
	}
}

func TestFinalizationOutboxFrozenCommitReplayAndProjectionReceipt(t *testing.T) {
	db, r, proof := foFixture(t)
	ctx := context.Background()
	input := foInput()
	record, err := r.CommitWithOwnerProof(ctx, input, proof)
	if err != nil {
		t.Fatal(err)
	}
	if record.FinalizationID != input.FinalizationID || record.ContentSnapshot != input.Content || record.PublicationStatus != "pending" || record.PublishedAt != nil {
		t.Fatal(record)
	}
	var body, status string
	var count int64
	if err := db.QueryRow("SELECT c.body,d.status,d.word_count FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=900001").Scan(&body, &status, &count); err != nil {
		t.Fatal(err)
	}
	if body != input.Content || status != "finalized" || count != finalizationDraftUnits(input.Content) {
		t.Fatal(body, status, count)
	}
	var raw, hash string
	if err := db.QueryRow("SELECT raw_value,source_hash FROM character_identity_proposals WHERE proposal_id=?", "fcs:"+input.FinalizationID).Scan(&raw, &hash); err != nil {
		t.Fatal(err)
	}
	if hash != summaryHash(raw) || !strings.Contains(raw, `"finalizationId":"synthetic-finalization"`) {
		t.Fatal("invalid frozen identity source", raw, hash)
	}
	before := foState(t, db)
	input.FinalizationID, input.TargetFileName, input.ExpectedContent = "new-unused-id", "new-unused.txt", nil
	replayed, err := r.CommitWithOwnerProof(ctx, input, proof)
	if err != nil || !reflect.DeepEqual(record, replayed) {
		t.Fatal(replayed, err)
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("replay changed immutable state")
	}
	pending, err := r.ListPending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ContentSnapshot != input.Content {
		t.Fatal(pending, err)
	}
	published, err := r.MarkPublishedWithOwnerProof(ctx, record.FinalizationID, proof)
	if err != nil || published.PublicationStatus != "published" || published.PublishedAt == nil || published.LastError != "" {
		t.Fatal(published, err)
	}
	pendingRecord, err := r.MarkPublicationPendingWithOwnerProof(ctx, record.FinalizationID, "synthetic retry failure 中文", proof)
	if err != nil || pendingRecord.PublicationStatus != "pending" || pendingRecord.PublishedAt == nil || *pendingRecord.PublishedAt != *published.PublishedAt {
		t.Fatal(pendingRecord, err)
	}
	if pendingRecord.FinalizationID != record.FinalizationID || pendingRecord.ContentSnapshot != record.ContentSnapshot || pendingRecord.ContentHash != record.ContentHash || pendingRecord.TargetFileName != record.TargetFileName {
		t.Fatal("projection changed immutable identity")
	}
}

func TestFinalizationOutboxExactCASAndInputBoundary(t *testing.T) {
	for _, name := range []string{"missing-baseline", "stale-body", "blob-body", "unknown-status", "chapter", "hash", "unsafe-id", "invalid-utf8", "path", "backslash-path", "empty-id"} {
		t.Run(name, func(t *testing.T) {
			db, r, proof := foFixture(t)
			input := foInput()
			expected := ErrFinalizationCommitConflict
			switch name {
			case "missing-baseline":
				input.ExpectedContent = nil
			case "stale-body":
				stale := "原稿\n😀\x00e\u0301 "
				input.ExpectedContent = &stale
			case "blob-body":
				mustExec(t, db, "UPDATE contents SET body=CAST(body AS BLOB) WHERE id=900001")
			case "unknown-status":
				mustExec(t, db, "UPDATE drafts SET status='unknown' WHERE id=900001")
			case "chapter":
				input.ChapterNumber++
				expected = ErrFinalizationChapterMismatch
			case "hash":
				input.ContentHash = strings.Repeat("0", 64)
				expected = ErrFinalizationInputInvalid
			case "unsafe-id":
				input.DraftID = 9007199254740992
				expected = ErrFinalizationInputInvalid
			case "invalid-utf8":
				input.Content = string([]byte{0xff})
				input.ContentHash = summaryHash(input.Content)
				expected = ErrFinalizationInputInvalid
			case "path":
				input.TargetFileName = "../outside.txt"
				expected = ErrFinalizationInputInvalid
			case "backslash-path":
				input.TargetFileName = `..\outside.txt`
				expected = ErrFinalizationInputInvalid
			case "empty-id":
				input.FinalizationID = "\ufeff "
				expected = ErrFinalizationInputInvalid
			}
			before := foState(t, db)
			if _, err := r.CommitWithOwnerProof(context.Background(), input, proof); !errors.Is(err, expected) {
				t.Fatalf("want %v got %v", expected, err)
			}
			if !reflect.DeepEqual(before, foState(t, db)) {
				t.Fatal("rejected commit changed state")
			}
		})
	}
}

func TestFinalizationOutboxRejectsContentPoolAliases(t *testing.T) {
	for _, owner := range []string{"drafts", "reviews", "revisions"} {
		t.Run(owner, func(t *testing.T) {
			db, r, proof := foFixture(t)
			switch owner {
			case "drafts":
				mustExec(t, db, "INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(10,1,'draft',900001)")
			case "reviews":
				mustExec(t, db, "INSERT INTO reviews(base_draft_id,review_index,content_id) VALUES(900001,1,900001)")
			case "revisions":
				mustExec(t, db, "INSERT INTO revisions(base_draft_id,revision_index,revision_type,content_id) VALUES(900001,1,'refine',900001)")
			}
			before := foState(t, db)
			if _, err := r.CommitWithOwnerProof(context.Background(), foInput(), proof); !errors.Is(err, ErrContentOwnershipConflict) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, foState(t, db)) {
				t.Fatal("aliased body mutated")
			}
		})
	}
}

func TestFinalizationOutboxSupersessionAndAtomicRollback(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			db, r, proof := foFixture(t)
			mustExec(t, db, "INSERT INTO contents(id,body) VALUES(900002,'previous finalized')")
			mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(900002,9,1,'finalized',900002)")
			mustExec(t, db, "UPDATE continuity_projection_meta SET generation=4,stale_from_chapter=2 WHERE id='main'")
			if abort {
				mustExec(t, db, "CREATE TRIGGER synthetic_abort_freeze BEFORE INSERT ON character_identity_proposals BEGIN SELECT RAISE(ABORT,'synthetic-finalization-freeze-failure'); END")
			}
			before := foState(t, db)
			_, err := r.CommitWithOwnerProof(context.Background(), foInput(), proof)
			if abort {
				if err == nil || !strings.Contains(err.Error(), "synthetic-finalization-freeze-failure") {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, foState(t, db)) {
					t.Fatal("late failure leaked finalized prose/outbox/supersession")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var status string
			var generation, stale int64
			if err := db.QueryRow("SELECT status FROM drafts WHERE id=900002").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT generation,stale_from_chapter FROM continuity_projection_meta WHERE id='main'").Scan(&generation, &stale); err != nil {
				t.Fatal(err)
			}
			if status != "superseded" || generation != 5 || stale != 2 {
				t.Fatal(status, generation, stale)
			}
		})
	}
}

func TestFinalizationOutboxConcurrentCASHasOneWinner(t *testing.T) {
	_, r, proof := foFixture(t)
	start := make(chan struct{})
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input := foInput()
			input.Content += fmt.Sprint(i)
			input.ContentHash = summaryHash(input.Content)
			input.FinalizationID += fmt.Sprint(i)
			<-start
			_, err := r.CommitWithOwnerProof(context.Background(), input, proof)
			failures <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(failures)
	wins, conflicts := 0, 0
	for err := range failures {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrFinalizationCommitConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
}

func TestFinalizationOutboxCancellationRollsBackReservedConnection(t *testing.T) {
	db, r, proof := foFixture(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	r.owner = foTestVerifier(func(context.Context, *sql.Conn, FinalizationScope, FinalizationMutation, any) error {
		cancel()
		return nil
	})
	before := foState(t, db)
	if _, err := r.CommitWithOwnerProof(ctx, foInput(), proof); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("cancelled operation wrote")
	}
	if _, err := db.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal("leaked manual transaction", err)
	}
	if _, err := db.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizationOutboxNoSchemaRepairAndNoForgedPublication(t *testing.T) {
	db, r, proof := foFixture(t)
	ctx := context.Background()
	mustExec(t, db, "DELETE FROM continuity_projection_meta WHERE id='main'")
	before := foState(t, db)
	if _, err := r.CommitWithOwnerProof(ctx, foInput(), proof); !errors.Is(err, ErrFinalizationSchemaUnsupported) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("silently initialized missing schema metadata")
	}
	mustExec(t, db, "INSERT INTO continuity_projection_meta(id) VALUES('main')")
	if _, err := r.CommitWithOwnerProof(ctx, foInput(), proof); err != nil {
		t.Fatal(err)
	}
	before = foState(t, db)
	for _, publish := range []bool{false, true} {
		var err error
		if publish {
			_, err = r.MarkPublishedWithOwnerProof(ctx, foInput().FinalizationID, nil)
		} else {
			_, err = r.MarkPublicationPendingWithOwnerProof(ctx, foInput().FinalizationID, "failure", nil)
		}
		if !errors.Is(err, ErrFinalizationOwnerRequired) {
			t.Fatal("publication accepted without owner receipt", err)
		}
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("forged publication changed projection")
	}
	mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot='tampered' WHERE draft_id=900001")
	if _, err := r.MarkPublishedWithOwnerProof(ctx, foInput().FinalizationID, proof); !errors.Is(err, ErrFinalizationSnapshotHashMismatch) {
		t.Fatal(err)
	}
}

func TestFinalizationDraftUnitsECMAScriptWhitespaceAndNativeUnicode(t *testing.T) {
	for _, fixture := range []struct {
		body  string
		count int64
	}{
		{"", 0}, {"中文𠮷", 3}, {"café cafe\u0301", 2}, {"can't l’amour", 2},
		{"\u0085\ufeff\u200b\x00\t\n!😀", 3}, {"a’中", 2}, {"123", 3}, {"\u0301a", 2},
	} {
		if got := finalizationDraftUnits(fixture.body); got != fixture.count {
			t.Fatalf("%q: %d != %d", fixture.body, got, fixture.count)
		}
	}
}
