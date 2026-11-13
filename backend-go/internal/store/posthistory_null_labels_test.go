package store

import (
	"context"
	"testing"
)

func TestPostHistoryNullableCreationLabelsPreserveStorage(t *testing.T) {
	db, repository, _ := phFixture(t)
	input := phInput()
	input["sourceLabel"] = nil
	input["steps"].([]any)[0].(map[string]any)["label"] = nil
	id, err := repository.CreateRun(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	rows := phRows(t, db, "SELECT source_label FROM post_process_runs WHERE id=?", id)
	if rows[0]["source_label"] != nil {
		t.Fatal("source label NULL changed", rows)
	}
	steps, err := repository.Steps(context.Background(), id)
	if err != nil || steps[0]["label"] != "" {
		t.Fatal("step read nullish default", steps, err)
	}
	raw := phRows(t, db, "SELECT label FROM post_process_steps WHERE run_id=? ORDER BY id", id)
	if raw[0]["label"] != nil {
		t.Fatal("step storage NULL changed", raw)
	}
	latest, err := repository.LatestRun(context.Background(), "arch_extract", "source-1")
	if err != nil || latest["sourceLabel"] != nil {
		t.Fatal("latest raw source label changed", latest, err)
	}
}
