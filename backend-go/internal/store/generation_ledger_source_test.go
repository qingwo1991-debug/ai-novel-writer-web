package store

import (
	"os"
	"strings"
	"testing"
)

// recordEffect mutates the receipt of one run's attempt. Two properties of it
// cannot be exercised by a store-level test: writing an effect needs a run and an
// attempt, and creating those needs a database built through the admission schema
// contract, which no test in this package can produce. So they are pinned at the
// source instead.
//
// Both properties were real defects, both were green in CI, and neither is
// visible from any other angle:
//
//   - the attempt lookup was keyed by attempt_id alone, so a caller able to
//     mutate one run could have an effect written into an attempt belonging to a
//     different run - the cross-run contamination the operation-scoped allowlist
//     exists to prevent;
//   - the receipt was returned for the run id where it takes an attempt id, so a
//     successful mutation answered GENERATION_ATTEMPT_MISSING.
func TestRecordEffectScopesItsAttemptToTheRun(t *testing.T) {
	source, err := os.ReadFile("generation_ledger.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, `case "recordEffect":`)
	if start < 0 {
		t.Fatal("recordEffect case not found")
	}
	end := strings.Index(text[start:], `case "replaceUnstartedBinding"`)
	if end < 0 {
		t.Fatal("end of the recordEffect case not found")
	}
	body := text[start : start+end]
	if !strings.Contains(body, "WHERE attempt_id=? AND run_id=?") {
		t.Fatal("recordEffect looks up its attempt without scoping it to the run")
	}
	if strings.Contains(body, "receipt(ctx, conn, text(0)") {
		t.Fatal("recordEffect returns its receipt for the run id, not the attempt id")
	}
}
