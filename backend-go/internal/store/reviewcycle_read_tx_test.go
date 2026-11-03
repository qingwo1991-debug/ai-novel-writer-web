package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestReviewCycleAbsentReadDoesNotHideSavedCycle(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE review_cycles(review_id INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadAbsentReviewCycleTx(ctx, tx, 7); err != nil {
		t.Fatalf("absent cycle: %v", err)
	}
	_ = tx.Rollback()
	if _, err := db.ExecContext(ctx, "INSERT INTO review_cycles(review_id) VALUES(7)"); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ReadAbsentReviewCycleTx(ctx, tx, 7); !errors.Is(err, ErrReviewCycleProjectionUnavailable) {
		t.Fatalf("saved cycle was hidden: %v", err)
	}
	if err := ReadAbsentReviewCycleTx(ctx, tx, 0); !errors.Is(err, ErrReviewRevisionInvalidInput) {
		t.Fatalf("invalid review id: %v", err)
	}
}
