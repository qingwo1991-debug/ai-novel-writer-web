package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestReviewRevisionFullReadTransactionMatchesRepository(t *testing.T) {
	db, repository, _ := rrFixture(t)
	ctx := context.Background()
	review, err := repository.CreateReview(ctx, rrInput())
	if err != nil {
		t.Fatal(err)
	}
	revision, err := repository.CreateRevision(ctx, rrInput())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, tc := range []struct {
		operation string
		id        int64
		want      func(context.Context, int64) (map[string]any, error)
	}{
		{"db:review-get-full", review["id"].(int64), repository.FullReview},
		{"db:revision-get-full", revision["id"].(int64), repository.FullRevision},
	} {
		got, err := ReadReviewRevisionFullTx(ctx, tx, tc.operation, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		want, err := tc.want(ctx, tc.id)
		if err != nil || !reflect.DeepEqual(got, want) || got["sourceDraft"] == nil {
			t.Fatalf("%s = %#v, %v; want %#v", tc.operation, got, err, want)
		}
	}
	if missing, err := ReadReviewRevisionFullTx(ctx, tx, "db:review-get-full", 999999); err != nil || missing != nil {
		t.Fatalf("missing full review = %#v, %v", missing, err)
	}
	if _, err := ReadReviewRevisionFullTx(ctx, tx, "db:revision-merge", 1); err == nil {
		t.Fatal("mutation accepted")
	}
	if wire := ReviewRevisionFullWire(nil); wire != nil {
		t.Fatalf("missing row = %#v", wire)
	}
	wire := ReviewRevisionFullWire(map[string]any{"content": []byte{0, 255}, "sourceDraft": nil})
	if !reflect.DeepEqual(wire["content"], map[string]any{"type": "Buffer", "data": []int{0, 255}}) {
		t.Fatalf("legacy full row = %#v", wire)
	}
}
