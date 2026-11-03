package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestReadDraftOperationUsesAdmittedTransactionAndOriginalDTO(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 2, 1, "finalized", "定稿正文")
	seed(t, db, 2, 1, 1, "draft", "草稿正文")
	seed(t, db, 3, 1, 2, "reviewed", "审稿正文")
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	list, _ := repository.ListByChapter(ctx, 1)
	all, _ := repository.ListAll(ctx)
	meta, _ := repository.GetMeta(ctx, 2)
	full, _ := repository.GetFull(ctx, 2)
	latest, _ := repository.GetLatestByChapter(ctx, 1)
	finalized, _ := repository.GetFinalizedByChapter(ctx, 2)
	maxFinalized, _ := repository.GetMaxFinalizedChapter(ctx)
	nextVersion, _ := repository.GetNextVersion(ctx, 1)
	for _, item := range []struct {
		operation string
		number    int64
		want      any
	}{
		{"db:draft-list", 1, list},
		{"db:draft-list-all", 0, all},
		{"db:draft-get-meta", 2, meta},
		{"db:draft-get-full", 2, full},
		{"db:draft-get-latest", 1, latest},
		{"db:draft-get-finalized", 2, finalized},
		{"db:draft-get-max-finalized-chapter", 0, maxFinalized},
		{"db:draft-next-version", 1, nextVersion},
	} {
		t.Run(item.operation, func(t *testing.T) {
			got, err := ReadDraftOperation(ctx, tx, item.operation, item.number)
			if err != nil || !reflect.DeepEqual(got, item.want) {
				t.Fatalf("read = %#v, %v; want %#v", got, err, item.want)
			}
		})
	}
	if got, err := ReadDraftOperation(ctx, tx, "db:draft-get-full", 999); err != nil || got != nil {
		t.Fatalf("missing draft = %#v, %v", got, err)
	}
	if _, err := ReadDraftOperation(ctx, tx, "db:draft-update-content", 2); err == nil {
		t.Fatal("write operation accepted by read transaction adapter")
	}
}
