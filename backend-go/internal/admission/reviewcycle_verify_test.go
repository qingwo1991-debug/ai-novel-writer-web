package admission

import (
	"context"
	"database/sql"
	"testing"
)

func TestM03JavaScriptWhitespaceParity(t *testing.T) {
	if m03Nonempty("\ufeff\u2000\t") {
		t.Fatal("BOM-only identifier accepted")
	}
	if !m03Nonempty("\u0085") {
		t.Fatal("U+0085 is not ECMAScript whitespace")
	}
	if got := m03CompactSQL(" A\ufeff B\u0085C "); got != "ab\u0085c" {
		t.Fatalf("ECMAScript /\\s/ mismatch: %q", got)
	}
}

func TestM03EscapedSurrogatePreScan(t *testing.T) {
	for _, value := range []string{`{"projectId":"\ud800"}`, `{"projectId":"\ud801"}`, `{"epoch":"\udc00"}`} {
		if !m03LoneSurrogateEscape(value) {
			t.Fatalf("isolated surrogate accepted: %s", value)
		}
	}
	for _, value := range []string{`{"projectId":"\ud83d\ude00"}`, `{"projectId":"\\ud800"}`} {
		if m03LoneSurrogateEscape(value) {
			t.Fatalf("valid JSON escape rejected: %s", value)
		}
	}
	if !m03LoneSurrogateEscape(string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})) {
		t.Fatal("invalid raw UTF-8 accepted before JSON decoding")
	}
}

func TestM03MergeSnapshotsRejectMissingAndOrphanCancellation(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"CREATE TABLE review_cycles(cycle_id TEXT PRIMARY KEY,revision_status TEXT NOT NULL)",
		"CREATE TABLE review_cycle_merges(cycle_id TEXT PRIMARY KEY,body TEXT NOT NULL)",
		"INSERT INTO review_cycles VALUES('committed','merge-committed')",
		"INSERT INTO review_cycle_merges VALUES('orphan','body')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	check := func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return m03VerifyMergeSnapshotCardinality(context.Background(), tx)
	}
	if err := check(); err == nil {
		t.Fatal("missing committed snapshot canceled by orphan")
	}
	if _, err := db.Exec("DELETE FROM review_cycle_merges WHERE cycle_id='orphan'"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("missing committed snapshot accepted")
	}
	if _, err := db.Exec("INSERT INTO review_cycle_merges VALUES('committed','body')"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO review_cycle_merges VALUES('orphan','body')"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("orphan snapshot accepted")
	}
}

func TestM03CycleBasicEvidenceRejectsTampering(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"CREATE TABLE generation_roots(root_action_id TEXT PRIMARY KEY,action_json TEXT NOT NULL)",
		"CREATE TABLE generation_runs(run_id TEXT PRIMARY KEY,root_action_id TEXT NOT NULL,binding_json TEXT NOT NULL)",
		"CREATE TABLE generation_attempts(attempt_id TEXT PRIMARY KEY,run_id TEXT NOT NULL,root_action_id TEXT NOT NULL)",
		"CREATE TABLE contents(id INTEGER PRIMARY KEY,body TEXT NOT NULL)",
		`CREATE TABLE reviews(id INTEGER PRIMARY KEY,base_draft_id INTEGER NOT NULL,
			source_draft_chapter_number INTEGER,source_draft_version INTEGER,source_draft_status TEXT,
			source_content TEXT,content_id INTEGER NOT NULL)`,
		`CREATE TABLE revisions(id INTEGER PRIMARY KEY,base_draft_id INTEGER NOT NULL,review_source_id INTEGER,
			source_draft_chapter_number INTEGER,source_draft_version INTEGER,source_draft_status TEXT,
			source_content TEXT,status TEXT,merged_to_draft_id INTEGER)`,
		m03ReviewCycleSQL,
		"INSERT INTO generation_roots VALUES('root','{\"projectId\":\"p\",\"epoch\":\"e\"}')",
		"INSERT INTO generation_runs VALUES('run','root','{\"projectId\":\"p\",\"epoch\":\"e\"}')",
		"INSERT INTO generation_attempts VALUES('attempt','run','root')",
		"INSERT INTO contents VALUES(1,'review body')",
		"INSERT INTO reviews VALUES(1,2,1,3,'draft','source text',1)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO review_cycles(cycle_id,root_action_id,review_id,review_content_hash,
		source_hash,finding_set_hash,comparison_version,revision_status,recheck_count)
		VALUES(?,?,?,?,?,?,1,'not-generated',0)`, "cycle", "root", 1, m03StringHash("review body"),
		m03StringHash("source text"), CanonicalM03FindingSetHash(nil)); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		_, err = m03VerifyCycleBasicEvidence(context.Background(), tx, "cycle")
		return err
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE generation_roots SET action_json='{"projectId":"\ud800","epoch":"e"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE generation_runs SET binding_json='{"projectId":"\ud801","epoch":"e"}'`); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("distinct isolated project surrogates compared equal")
	}
	if _, err := db.Exec(`UPDATE generation_roots SET action_json='{"projectId":"p","epoch":"e"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE generation_runs SET binding_json='{"projectId":"p","epoch":"e"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE generation_runs SET binding_json='{\"projectId\":\"other\",\"epoch\":\"e\"}'"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("cross-project attempt binding accepted")
	}
	if _, err := db.Exec("UPDATE generation_runs SET binding_json='{\"projectId\":\"p\",\"epoch\":\"e\"}'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE contents SET body='tampered'"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("tampered review body accepted")
	}
	for _, statement := range []string{
		"UPDATE contents SET body='review body'",
		"INSERT INTO contents VALUES(2,'confirmation')",
		"INSERT INTO contents VALUES(3,'revision body')",
		"INSERT INTO reviews VALUES(2,2,1,3,'draft','source text',2)",
		"INSERT INTO revisions VALUES(3,2,2,1,3,'draft','source text','pending',NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE review_cycles SET confirmation_review_id=2,confirmation_content_hash=?,
		revision_id=3,revision_status='generated' WHERE cycle_id='cycle'`, m03StringHash("confirmation")); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE revisions SET source_content='other'"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("tampered revision source accepted")
	}
	for _, statement := range []string{
		"UPDATE revisions SET source_content='source text',status='merged',merged_to_draft_id=2",
		"CREATE TABLE review_cycle_merges(cycle_id TEXT PRIMARY KEY,body TEXT NOT NULL)",
		"INSERT INTO review_cycle_merges VALUES('cycle','merged body')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE review_cycles SET revision_status='merge-committed',merged_hash=?", m03StringHash("merged body")); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	// Model a corrupted database that bypassed SQLite's CHECK constraint.
	if _, err := db.Exec("PRAGMA ignore_check_constraints=ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE review_cycles SET recheck_count=1,recheck_attempt_id=NULL"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("recheck count without attempt accepted")
	}
	if _, err := db.Exec("UPDATE review_cycles SET recheck_count=0,recheck_attempt_id='attempt'"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("recheck attempt without count accepted")
	}
}

func TestCanonicalM03FindingSetHashTypeScriptVector(t *testing.T) {
	if got := CanonicalM03FindingSetHash(nil); got != "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945" {
		t.Fatalf("empty TypeScript vector: %s", got)
	}
	one, two := int64(1), int64(2)
	unit, excerpt, target, expected := "utf16-code-unit", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "x", "good"
	findings := []M03FindingIdentity{
		{FindingID: "😀", SourceProjectID: "项<目", SourceEpoch: "e", SourceID: "draft:1", SourceRevision: 2,
			SourceContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Category:          "情节", Kind: "literary", ProblemText: "换行\n测试"},
		{FindingID: "z", SourceProjectID: "p", SourceEpoch: "e", SourceID: "draft:1", SourceRevision: 1,
			SourceContentHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SpanStart:         &one, SpanEnd: &two, SpanUnit: &unit, ExcerptHash: &excerpt, Occurrence: &one,
			Category: "logic", TargetID: &target, Kind: "objective", ProblemText: "bad", ExpectedText: &expected},
	}
	const want = "da7d7965724b8159c3ea3171ebcd647c3709046d1719e3589967fcb1ebf28ecc"
	if got := CanonicalM03FindingSetHash(findings); got != want {
		t.Fatalf("TypeScript vector: %s", got)
	}
	if got := CanonicalM03FindingSetHash([]M03FindingIdentity{findings[1], findings[0]}); got != want {
		t.Fatalf("canonical identity order: %s", got)
	}
}

func TestVerifyM03ReviewCycleSchema(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE generation_roots(root_action_id TEXT PRIMARY KEY)",
		"CREATE TABLE generation_attempts(attempt_id TEXT PRIMARY KEY)",
		"CREATE TABLE reviews(id INTEGER PRIMARY KEY)",
		"CREATE TABLE revisions(id INTEGER PRIMARY KEY)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(m03ReviewCycleSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE review_cycle_merges (cycle_id TEXT PRIMARY KEY REFERENCES review_cycles(cycle_id) ON DELETE CASCADE, body TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	check := func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := VerifyM03ReviewCycleSchema(context.Background(), tx); err != nil {
			return err
		}
		return VerifyM03FindingSetHash(context.Background(), tx, "absent", "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945")
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyM03FindingSetHash(context.Background(), tx, "absent", "forged"); err == nil {
		t.Fatal("forged finding set hash accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ALTER TABLE review_findings ADD COLUMN forged TEXT"); err != nil {
		t.Fatal(err)
	}
	if err := check(); err == nil {
		t.Fatal("forged M03 column accepted")
	}
}
