package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestReviewRevisionReadTransactionMatchesRepositoryMetadata(t *testing.T) {
	db, repository, _ := rrFixture(t)
	ctx := context.Background()
	if _, err := repository.CreateReview(ctx, rrInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRevision(ctx, rrInput()); err != nil {
		t.Fatal(err)
	}
	beforeReviews, err := repository.ListReviews(ctx, 900001)
	if err != nil {
		t.Fatal(err)
	}
	beforePending, err := repository.ListRevisions(ctx, 900001, true)
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
		want      []map[string]any
	}{
		{"db:review-list", beforeReviews},
		{"db:revision-get-pending", beforePending},
	} {
		got, err := ReadReviewRevisionListTx(ctx, tx, tc.operation, 900001)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s = %#v, %v; want %#v", tc.operation, got, err, tc.want)
		}
	}
	if got, err := ReadReviewRevisionListTx(ctx, tx, "db:review-list", 999999); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing reviews = %#v, %v", got, err)
	}
	if _, err := ReadReviewRevisionListTx(ctx, tx, "db:revision-merge", 900001); err == nil {
		t.Fatal("mutation operation accepted by read-only adapter")
	}
}

func TestReviewRevisionListWirePreservesLegacyBlob(t *testing.T) {
	wire := ReviewRevisionListWire([]map[string]any{{"id": int64(1), "userPrompt": []byte{0, 255, 34}, "reviewSourceId": nil}})
	if len(wire) != 1 || !reflect.DeepEqual(wire[0]["userPrompt"], map[string]any{"type": "Buffer", "data": []int{0, 255, 34}}) || wire[0]["reviewSourceId"] != nil {
		t.Fatalf("legacy metadata = %#v", wire)
	}
	if empty := ReviewRevisionListWire(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("empty list = %#v", empty)
	}
}
