package store

// These tests drive the review-cycle writer of review_cycles.go against the pinned
// v7 schema: the review row, its cycle row, its findings and the effect stamped on
// the run's attempt, all in the one BEGIN IMMEDIATE transaction the writer owns.
//
// What is proved here, and why each proof has the shape it does:
//
//   - The finding descriptors are pinned to the read projection. The two finding
//     ids in TestReviewCycleBuildFindingsMatchesProjectionIDs are the ones the
//     TypeScript ordered-JSON identity produces for this fixture (they are the same
//     two ids reviewcycle_projection_test.go asserts), so a writer that described a
//     finding even slightly differently than the reader would land rows no read
//     projection can address. The same test compares the written set against
//     rcItemIndex, which is the reader's own map.
//   - The descriptor test compares rccDescribeFinding against rcFindingID, the
//     identity function the projection already uses, for the four shapes a finding
//     can take: a goal with one evidence slice (anchored), the same goal reported
//     unknown (same id, different status), a free item whose quote is absent from
//     the chapter (unanchored), and a preflight item that is anchored by its stable
//     fact key. It also pins the UTF-16 span of an emoji-prefixed chapter.
//   - The happy path is read back through a second connection opened with mode=ro.
//     The writer's own two connections are not used for any assertion, so a
//     transaction that only saw its own writes cannot pass.
//   - Every refusal is bracketed by a digest of the whole database file and by a
//     read-only count of the three tables the commit writes. A refusal that wrote
//     and rolled back would still change neither, but a refusal that committed a
//     partial transaction would change both.
//   - The replay arm proves the idempotent branch: the second call answers the same
//     receipt, and the file is byte for byte what the first call left.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

const (
	// The artifact the run settled: a version 2 review report in the model's own
	// output shape (reviewreport.Parse reads summary/items/goalReviews).
	rccTestArtifact = `{"summary":"审稿通过","items":[{"category":"风格","severity":"warning","description":"这段描写可以更紧凑","quote":"灯影摇晃"}]}`
	// One report, read by both halves below: reviewcycle_projection_test.go uses the
	// same body for the same two ids.
	rccProjectionReport = `{"summary":"审稿","items":[{"category":"目标","severity":"error","description":"目标未达成","quote":"目标","goalId":"ch1:mustShow:1"},{"category":"风格","severity":"warning","description":"重写段落","quote":"不存在"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"ch1:mustShow:1","text":"展示目标","status":"unmet","description":"必须出现","evidence":[{"quote":"目标","start":2,"end":4}]}]}}`
	// The ids the TypeScript identity produces for rccProjectionReport over
	// rccDescriptorSourceText: the goal item first, the free item second.
	rccProjectionGoalID = "finding:d486f3f8e124b0bfe1c8cf9ef5a8f560e82d0cff96b96d273d6e2b52a9752cc3"
	rccProjectionFreeID = "finding:0e7a89fa3621add6d9efdbffde4fca2b3cfbd48f90ba500958e23890c098c477"
	// The frozen chapter is rrctxChapterOne, whose second sentence quotes once.
	rccTestModelID    = "fixture-review-model"
	rccTestEpoch      = "epoch-review-commit"
	rccTestRootID     = "root-review-commit"
	rccTestRunID      = "run-review-commit"
	rccTestAttemptID  = "attempt-review-commit"
	rccTestArtifactID = "artifact-review-commit"
)

// rccDescriptorSourceText is the chapter every identity below is computed over: one
// emoji (two UTF-16 units), so an identity built from byte offsets would disagree.
const rccDescriptorSourceText = "😀目标！"

// rccDescriptorSource is the frozen source identity every identity below is
// computed over: project-A/epoch-A/draft:7 revision 2, exactly the fixtures
// reviewcycle_projection_test.go pins its ids against.
func rccDescriptorSource() reviewCycleSource {
	return reviewCycleSource{ProjectID: "project-A", Epoch: "epoch-A", ID: "draft:7", Revision: 2,
		ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

// rccCommitCase is one project database with a settled review run over a frozen
// review-chapter context: everything CommitReview reads, and nothing it writes.
type rccCommitCase struct {
	t       *testing.T
	db      *sql.DB
	path    string
	frozen  *FrozenReviewRevisionContext
	request ReviewCommitRequest
}

// rccCommitFixture provisions the schema, one revised draft, its frozen context,
// the root action, the run that froze that context, and the settled attempt and
// artifact the run is allowed to commit.
func rccCommitFixture(t *testing.T) *rccCommitCase {
	t.Helper()
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
	rrctxBlueprintRow(t, db, 1, "开门；见客\n点灯", `["张三"]`)
	frozen := rrctxCaptureOK(t, NewReviewRevisionContexts(db), path,
		rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	fingerprint := plotObject{{"modelId", rccTestModelID}, {"policyRevision", int64(3)}}
	action := plotObject{
		{"rootActionId", rccTestRootID},
		{"projectId", rrctxTestProjectID},
		{"epoch", rccTestEpoch},
		{"operation", ReviewRevisionOperationReviewChapter},
		{"status", "active"},
	}
	budget := plotObject{{"policy", plotObject{{"maxAttempts", int64(3)}}}}
	mustExec(t, db, `INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json)
 VALUES(?,?,?,?)`, rccTestRootID, "idem-review-commit", rrctxJSON(t, action), rrctxJSON(t, budget))
	// The frozen manifest the begin wrote: the producer's four fields, carried as the
	// same bytes the producer emitted (SourceManifestFields embeds the parsed value,
	// and plotJSON reproduces Text byte for byte).
	binding := plotObject{
		{"projectId", rrctxTestProjectID},
		{"epoch", rccTestEpoch},
		{"fingerprint", fingerprint},
		{"sourceManifest", frozen.SourceManifestFields()},
	}
	mustExec(t, db, `INSERT INTO generation_runs(run_id,root_action_id,binding_json,status,created_at_ms,open_key)
 VALUES(?,?,?,?,?,?)`, rccTestRunID, rccTestRootID, rrctxJSON(t, binding), "running", 1, "open-review-commit")
	attempt := plotObject{{"attemptId", rccTestAttemptID}, {"runId", rccTestRunID}, {"status", "settled"}}
	usage := plotObject{
		{"purpose", ReviewRevisionOperationReviewChapter},
		{"result", plotObject{{"finishReason", "stop"}}},
		{"artifactIdentity", plotObject{
			{"artifactId", rccTestArtifactID},
			{"epoch", rccTestEpoch},
			{"fingerprint", fingerprint},
		}},
	}
	mustExec(t, db, `INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
 attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`,
		rccTestAttemptID, "reservation-review-commit", rccTestRunID, rccTestRootID,
		rrctxJSON(t, attempt), rrctxJSON(t, usage), "nonce-review-commit")
	artifact := plotObject{
		{"artifactId", rccTestArtifactID},
		{"attemptId", rccTestAttemptID},
		{"rootActionId", rccTestRootID},
		{"projectId", rrctxTestProjectID},
		{"epoch", rccTestEpoch},
		{"fingerprint", fingerprint},
		{"revision", int64(0)},
		{"textHash", rrctxSHA(rccTestArtifact)},
		{"text", rccTestArtifact},
	}
	mustExec(t, db, `INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status)
 VALUES(?,?,?,?,?,?)`, rccTestArtifactID, rccTestAttemptID, rccTestRunID, rrctxJSON(t, artifact), 0, "settled")
	handle := ReviewCommitHandle{
		ProjectID:    rrctxTestProjectID,
		Epoch:        rccTestEpoch,
		RootActionID: rccTestRootID,
		RunID:        rccTestRunID,
	}
	artifactRef := ReviewCommitArtifact{
		ArtifactID: rccTestArtifactID,
		Revision:   0,
		TextHash:   rrctxSHA(rccTestArtifact),
	}
	fixture := &rccCommitCase{t: t, db: db, path: path, frozen: frozen}
	fixture.request = ReviewCommitRequest{
		ProjectID: rrctxTestProjectID,
		LiveEpoch: rccTestEpoch,
		Handle:    handle,
		Artifact:  artifactRef,
	}
	// The adapter's own registry proof, reduced to the one comparison that matters
	// here: the run's operation and the frozen bytes it carries.
	fixture.request.AssertPrepared = func(operation, text string) error {
		if operation != ReviewRevisionOperationReviewChapter || text != frozen.Text {
			return ErrReviewCommitContextRequired
		}
		return nil
	}
	return fixture
}

// rccReadOnly opens the same file with mode=ro, so every assertion below is on what
// another connection can see rather than on the writer's own transaction.
func rccReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("read-only connection: %v", err)
	}
	return db
}

// rccCommit runs the writer once over the fixture.
func (f *rccCommitCase) commit(request ReviewCommitRequest) (*ReviewCommitReceipt, error) {
	f.t.Helper()
	return NewReviewRevisions(f.db).CommitReview(context.Background(), request)
}

// rccCounts is how many rows each of the three written tables holds, read over a
// connection that only sees committed state.
func rccCounts(t *testing.T, db *sql.DB) [3]int64 {
	t.Helper()
	var counts [3]int64
	for index, table := range []string{"reviews", "review_cycles", "review_findings"} {
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&counts[index]); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
	}
	return counts
}

// rccRefused proves one refusal by its exact sentinel and by an untouched database:
// the file bytes are identical, and none of the three written tables gained a row.
func (f *rccCommitCase) refused(label string, request ReviewCommitRequest, want error) {
	f.t.Helper()
	before := rrctxDigest(f.t, f.path)
	counts := rccCounts(f.t, rccReadOnly(f.t, f.path))
	receipt, err := f.commit(request)
	if err == nil {
		f.t.Fatalf("%s: expected refusal %v, but the commit succeeded", label, want)
	}
	if !errors.Is(err, want) {
		f.t.Fatalf("%s: expected %v, got %v", label, want, err)
	}
	if receipt != nil {
		f.t.Fatalf("%s: a refused commit returned a receipt", label)
	}
	if after := rrctxDigest(f.t, f.path); after != before {
		f.t.Fatalf("%s: %v changed the database", label, want)
	}
	if after := rccCounts(f.t, rccReadOnly(f.t, f.path)); after != counts {
		f.t.Fatalf("%s: %v wrote rows: %v -> %v", label, want, counts, after)
	}
}

// rccRowValue reads one integer or text column, so an assertion never depends on a
// driver conversion it did not name.
func rccRowValue(t *testing.T, db *sql.DB, query string, args ...any) any {
	t.Helper()
	var value any
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read %s: %v", query, err)
	}
	return value
}

// TestReviewCycleFindingDescriptorMatchesProjectionIdentity pins the writer's
// descriptor to the reader's identity function for every shape a finding takes.
func TestReviewCycleFindingDescriptorMatchesProjectionIdentity(t *testing.T) {
	source := rccDescriptorSource()
	text := rccDescriptorSourceText
	goalID := "ch1:mustShow:1"
	quote := "目标"
	goal := reviewCycleSavedGoal{Index: 0, ID: goalID, Text: "展示目标", Description: "必须出现",
		Status: "unmet", Evidence: []reviewCycleGoalEvidence{{Quote: quote, Start: 2, End: 4}}}
	item := reviewCycleSavedItem{Index: 0, Category: "目标", Severity: "error",
		Description: "目标未达成", Quote: &quote, GoalID: &goalID}

	// A goal with exactly one evidence slice is anchored, and its status comes from
	// the goal review rather than from the item's severity.
	finding, err := rccDescribeFinding(source, text, item, &goal)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := rcFindingID(source, text, item, &goal)
	if err != nil {
		t.Fatal(err)
	}
	if finding.ID != projection {
		t.Fatalf("goal identity drifted: writer %s, reader %s", finding.ID, projection)
	}
	if finding.Status != "unresolved" || finding.Kind != "objective" ||
		finding.Category != "目标" || finding.ProblemText != "目标未达成" {
		t.Fatalf("goal descriptor: %v", finding)
	}
	if finding.TargetID == nil || *finding.TargetID != goalID {
		t.Fatalf("goal target: %v", finding.TargetID)
	}
	if finding.ExpectedText == nil || *finding.ExpectedText != "展示目标\n必须出现" {
		t.Fatalf("goal expected text: %v", finding.ExpectedText)
	}
	// The span is UTF-16: the emoji before 目标 is two units wide, not four bytes.
	if finding.SpanStart == nil || *finding.SpanStart != 2 || finding.SpanEnd == nil || *finding.SpanEnd != 4 {
		t.Fatalf("goal span: %v..%v", finding.SpanStart, finding.SpanEnd)
	}
	if finding.SpanUnit == nil || *finding.SpanUnit != "utf16-code-unit" ||
		finding.ExcerptHash == nil || *finding.ExcerptHash != recoveryHash(quote) ||
		finding.Occurrence == nil || *finding.Occurrence != 1 {
		t.Fatalf("goal anchor: %v %v %v", finding.SpanUnit, finding.ExcerptHash, finding.Occurrence)
	}
	// The status is not part of the identity: an unknown goal keeps its id.
	unknown := goal
	unknown.Status = "unknown"
	unknownFinding, err := rccDescribeFinding(source, text, item, &unknown)
	if err != nil {
		t.Fatal(err)
	}
	if unknownFinding.Status != "unknown" || unknownFinding.ID != finding.ID {
		t.Fatalf("unknown goal: status %v id %v", unknownFinding.Status, unknownFinding.ID)
	}

	// A free item anchors only through a stable fact key: with a target but no quote
	// in the chapter it is unverified, and every anchor column stays NULL.
	absent := "不存在"
	free := reviewCycleSavedItem{Index: 1, Category: "风格", Severity: "warning",
		Description: "重写段落", Quote: &absent}
	freeFinding, err := rccDescribeFinding(source, text, free, nil)
	if err != nil {
		t.Fatal(err)
	}
	freeProjection, err := rcFindingID(source, text, free, nil)
	if err != nil {
		t.Fatal(err)
	}
	if freeFinding.ID != freeProjection || freeFinding.Status != "unverified" || freeFinding.Kind != "literary" {
		t.Fatalf("free descriptor: %v", freeFinding)
	}
	if freeFinding.TargetID != nil || freeFinding.SpanStart != nil || freeFinding.SpanEnd != nil ||
		freeFinding.SpanUnit != nil || freeFinding.ExcerptHash != nil || freeFinding.Occurrence != nil ||
		freeFinding.ExpectedText != nil {
		t.Fatalf("unverified descriptor carries an anchor: %v", freeFinding)
	}

	// The same item with a stable fact key is anchored at the quote it names.
	factKey := "fact:6c9ed09cbd23ee2c"
	preflight := reviewCycleSavedItem{Index: 2, Category: "事实", Severity: "warning",
		Description: "死者又出现", Quote: &quote, StableFactKey: &factKey}
	preflightFinding, err := rccDescribeFinding(source, text, preflight, nil)
	if err != nil {
		t.Fatal(err)
	}
	preflightProjection, err := rcFindingID(source, text, preflight, nil)
	if err != nil {
		t.Fatal(err)
	}
	if preflightFinding.ID != preflightProjection || preflightFinding.Status != "unresolved" ||
		preflightFinding.Kind != "objective" || preflightFinding.TargetID == nil ||
		*preflightFinding.TargetID != factKey {
		t.Fatalf("preflight descriptor: %v", preflightFinding)
	}
	if preflightFinding.SpanStart == nil || *preflightFinding.SpanStart != 2 ||
		preflightFinding.ExcerptHash == nil || *preflightFinding.ExcerptHash != recoveryHash(quote) {
		t.Fatalf("preflight anchor: %v %v", preflightFinding.SpanStart, preflightFinding.ExcerptHash)
	}

	// A quote that occurs twice cannot be anchored even with a target: the reader
	// drops the anchor, and the writer must drop it identically.
	overlap := reviewCycleSavedItem{Index: 3, Category: "事实", Severity: "warning",
		Description: "重复引用", Quote: &quote, StableFactKey: &factKey}
	overlapFinding, err := rccDescribeFinding(source, "目标目标", overlap, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlapProjection, err := rcFindingID(source, "目标目标", overlap, nil)
	if err != nil {
		t.Fatal(err)
	}
	if overlapFinding.ID != overlapProjection || overlapFinding.Status != "unverified" ||
		overlapFinding.TargetID != nil {
		t.Fatalf("overlapping descriptor: %v", overlapFinding)
	}

	// The identity the canonical set hash is computed from names the source itself.
	identity := finding.identity(source)
	if identity.SourceProjectID != source.ProjectID || identity.SourceEpoch != source.Epoch ||
		identity.SourceID != source.ID || identity.SourceRevision != source.Revision ||
		identity.SourceContentHash != source.ContentHash || identity.FindingID != finding.ID ||
		identity.ProblemText != finding.ProblemText {
		t.Fatalf("identity: %+v", identity)
	}
}

// TestReviewCycleBuildFindingsMatchesProjectionIDs pins the written set to the ids
// the reader computes, and to the ids the TypeScript identity produces.
func TestReviewCycleBuildFindingsMatchesProjectionIDs(t *testing.T) {
	source := rccDescriptorSource()
	findings, hash, err := rccBuildFindings(rccProjectionReport, rccDescriptorSourceText, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("finding count = %d", len(findings))
	}
	// Goals are described before free items, exactly as the reference's buildFindings
	// orders them.
	if findings[0].ID != rccProjectionGoalID || findings[1].ID != rccProjectionFreeID {
		t.Fatalf("ordered ids: %s %s", findings[0].ID, findings[1].ID)
	}
	if findings[0].Status != "unresolved" || findings[1].Status != "unverified" {
		t.Fatalf("ordered status: %v %v", findings[0].Status, findings[1].Status)
	}
	// The reader's own map addresses exactly the rows this writer would insert.
	indices, err := rcItemIndex(rccProjectionReport, rccDescriptorSourceText, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(indices) != len(findings) {
		t.Fatalf("reader maps %d findings, writer built %d", len(indices), len(findings))
	}
	for _, finding := range findings {
		if _, ok := indices[finding.ID]; !ok {
			t.Fatalf("reader does not address %s", finding.ID)
		}
	}
	// The set hash is the canonical hash of exactly these findings, in either order.
	identities := []admission.M03FindingIdentity{findings[0].identity(source), findings[1].identity(source)}
	if hash != admission.CanonicalM03FindingSetHash(identities) {
		t.Fatalf("set hash: %s", hash)
	}
	reversed := []admission.M03FindingIdentity{identities[1], identities[0]}
	if hash != admission.CanonicalM03FindingSetHash(reversed) {
		t.Fatal("set hash depends on insertion order")
	}
	if len(hash) != 64 {
		t.Fatalf("set hash length = %d", len(hash))
	}
}

// TestReviewCycleBuildFindingsRefusesBrokenReport proves the writer refuses a report
// whose evidence no longer names its own quote, rather than anchoring at a byte
// offset it did not verify.
func TestReviewCycleBuildFindingsRefusesBrokenReport(t *testing.T) {
	// The evidence claims the quote starts at unit 0, which over this chapter is the
	// emoji: the slice no longer spells 目标.
	const broken = `{"summary":"审稿","items":[{"category":"目标","severity":"error","description":"目标未达成","quote":"目标","goalId":"ch1:mustShow:1"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"complete","items":[{"id":"ch1:mustShow:1","text":"展示目标","status":"unmet","description":"必须出现","evidence":[{"quote":"目标","start":0,"end":2}]}]}}`
	if _, _, err := rccBuildFindings(broken, rccDescriptorSourceText, rccDescriptorSource()); !errors.Is(err, errReviewCycleSavedReport) {
		t.Fatalf("accepted a report whose evidence does not spell its quote: %v", err)
	}
	// A report whose only item is a pass has no findings at all, and the empty set
	// hashes to the canonical empty array rather than to nothing.
	const passing = `{"summary":"审稿","items":[{"category":"风格","severity":"pass","description":"可以定稿"}],"goalReview":{"version":1,"chapterNumber":1,"coverage":"not_configured","items":[]}}`
	findings, hash, err := rccBuildFindings(passing, rccDescriptorSourceText, rccDescriptorSource())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("a pass wrote %d findings", len(findings))
	}
	if hash != admission.CanonicalM03FindingSetHash(nil) {
		t.Fatalf("empty set hash: %s", hash)
	}
}

// TestReviewCommitReviewWritesTheCycle is the happy path, read back over a
// connection the writer never touched.
func TestReviewCommitReviewWritesTheCycle(t *testing.T) {
	fixture := rccCommitFixture(t)
	receipt, err := fixture.commit(fixture.request)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if receipt == nil || !receipt.Success || receipt.Kind != "review" {
		t.Fatalf("receipt: %+v", receipt)
	}
	if receipt.ID < 1 || receipt.Index != 1 {
		t.Fatalf("receipt identity: id %d index %d", receipt.ID, receipt.Index)
	}
	if receipt.Content == "" || receipt.ContentHash != recoveryHash(receipt.Content) {
		t.Fatalf("receipt content: %d bytes, hash %s", len(receipt.Content), receipt.ContentHash)
	}
	if receipt.Source.ID != 1 || receipt.Source.ChapterNumber != 1 || receipt.Source.Version != 1 ||
		receipt.Source.Status != "revised" || receipt.Source.Content != rrctxChapterOne {
		t.Fatalf("receipt source: %+v", receipt.Source)
	}

	read := rccReadOnly(t, fixture.path)
	// The review row itself carries the body and the source the receipt names.
	if got := rccRowValue(t, read, "SELECT base_draft_id FROM reviews WHERE id=?", receipt.ID); got != int64(1) {
		t.Fatalf("review base draft: %v", got)
	}
	if got := rccRowValue(t, read, "SELECT body FROM contents WHERE id=(SELECT content_id FROM reviews WHERE id=?)",
		receipt.ID); got != receipt.Content {
		t.Fatalf("review body is not the receipt content")
	}
	// The cycle row: not-generated, one finding set, no confirmation, no revision.
	var cycleID, findingSetHash string
	var comparisonVersion, recheckCount int64
	var revisionStatus string
	if err := read.QueryRow(`SELECT cycle_id,finding_set_hash,comparison_version,revision_status,recheck_count
 FROM review_cycles WHERE review_id=?`, receipt.ID).Scan(&cycleID, &findingSetHash, &comparisonVersion,
		&revisionStatus, &recheckCount); err != nil {
		t.Fatalf("cycle row: %v", err)
	}
	if revisionStatus != "not-generated" || comparisonVersion != 1 || recheckCount != 0 {
		t.Fatalf("cycle state: %s v%d rechecks %d", revisionStatus, comparisonVersion, recheckCount)
	}
	for _, column := range []string{"confirmation_review_id", "confirmation_content_hash", "revision_id",
		"merged_hash", "recheck_attempt_id"} {
		if got := rccRowValue(t, read, "SELECT "+column+" FROM review_cycles WHERE review_id=?", receipt.ID); got != nil {
			t.Fatalf("cycle %s = %v", column, got)
		}
	}
	expectedCycleID := "cycle:" + recoveryHash(rrctxJSON(t, []any{rccTestRootID, receipt.ID, receipt.ContentHash}))
	if cycleID != expectedCycleID {
		t.Fatalf("cycle id: %s", cycleID)
	}
	if got := rccRowValue(t, read, "SELECT root_action_id FROM review_cycles WHERE review_id=?",
		receipt.ID); got != rccTestRootID {
		t.Fatalf("cycle root: %v", got)
	}
	if got := rccRowValue(t, read, "SELECT source_hash FROM review_cycles WHERE review_id=?",
		receipt.ID); got != rrctxSHA(rrctxChapterOne) {
		t.Fatalf("cycle source hash: %v", got)
	}

	// Every non-pass item of the report becomes a row, exactly as the reference
	// inserts them (electron/repositories/review-cycle-repository.ts:615-624 loops
	// over buildFindings(report, ...)). The fixture's report carries four anchored
	// preflight findings and one free warning, so five rows land and only the
	// warning is unanchored - that row is the one this test follows.
	if len(rows) != 5 {
		t.Fatalf("finding rows = %d", len(rows))
	}
	var row map[string]any
	anchored := 0
	for _, candidate := range rows {
		if candidate["target_id"] == nil {
			if row != nil {
				t.Fatalf("more than one unanchored finding: %v and %v", row, candidate)
			}
			row = candidate
			continue
		}
		anchored++
		for _, column := range []string{"span_start", "span_end", "span_unit", "excerpt_hash"} {
			if candidate[column] == nil {
				t.Fatalf("anchored finding %v carries no %s", candidate["finding_id"], column)
			}
		}
	}
	if row == nil || anchored != 4 {
		t.Fatalf("the free warning is missing: %d anchored", anchored)
	}
	if glText(row["source_project_id"]) != rrctxTestProjectID || glText(row["source_epoch"]) != rccTestEpoch ||
		glText(row["source_id"]) != "draft:1" || glText(row["category"]) != "风格" ||
		glText(row["kind"]) != "literary" || glText(row["status"]) != "unverified" ||
		glText(row["problem_text"]) != "这段描写可以更紧凑" {
		t.Fatalf("finding row: %v", row)
	}
	sourceRevision, revisionOK := rrInteger(row["source_revision"], 0)
	if !revisionOK || sourceRevision != 1 || glText(row["source_content_hash"]) != rrctxSHA(rrctxChapterOne) {
		t.Fatalf("finding source: %v %v", row["source_revision"], row["source_content_hash"])
	}
	for _, column := range []string{"span_start", "span_end", "span_unit", "excerpt_hash", "occurrence",
		"target_id", "expected_text", "evidence_hash", "confirmation_item_index"} {
		if row[column] != nil {
			t.Fatalf("unverified finding carries %s = %v", column, row[column])
		}
	}
	// The stored set hash covers exactly the stored rows, recomputed here without the
	// writer's own reader.
	identities := make([]admission.M03FindingIdentity, 0, len(rows))
	for _, stored := range rows {
		revision, ok := rrInteger(stored["source_revision"], 0)
		if !ok {
			t.Fatalf("stored finding revision: %v", stored["source_revision"])
		}
		identities = append(identities, admission.M03FindingIdentity{
			FindingID:         glText(stored["finding_id"]),
			SourceProjectID:   glText(stored["source_project_id"]),
			SourceEpoch:       glText(stored["source_epoch"]),
			SourceID:          glText(stored["source_id"]),
			SourceRevision:    revision,
			SourceContentHash: glText(stored["source_content_hash"]),
			Category:          glText(stored["category"]),
			Kind:              glText(stored["kind"]),
			ProblemText:       glText(stored["problem_text"]),
		})
	}
	if findingSetHash != admission.CanonicalM03FindingSetHash(identities) {
		t.Fatalf("stored set hash does not cover the stored rows")
	}
	identity := admission.M03FindingIdentity{
		FindingID:         glText(row["finding_id"]),
		SourceProjectID:   glText(row["source_project_id"]),
		SourceEpoch:       glText(row["source_epoch"]),
		SourceID:          glText(row["source_id"]),
		SourceRevision:    sourceRevision,
		SourceContentHash: glText(row["source_content_hash"]),
		Category:          glText(row["category"]),
		Kind:              glText(row["kind"]),
		ProblemText:       glText(row["problem_text"]),
	}
	// The receipt summarises the same row the reader maps.
	written := reviewCycleSource{rrctxTestProjectID, rccTestEpoch, "draft:1", 1, rrctxSHA(rrctxChapterOne)}
	indices, err := rcItemIndex(receipt.Content, rrctxChapterOne, written)
	if err != nil {
		t.Fatalf("reader projection of the written body: %v", err)
	}
	// The reader must address the warning the writer stored. How many other items
	// it maps is the reader's own contract, covered by the projection test above,
	// so this does not pin a total that a five-finding report would change.
	if _, ok := indices[identity.FindingID]; !ok {
		t.Fatalf("reader does not address the written finding: %v", indices)
	}

	// The effect stamped on the attempt names the review it wrote, and nothing else.
	var usage string
	if err := read.QueryRow("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?",
		rccTestAttemptID).Scan(&usage); err != nil {
		t.Fatalf("attempt usage: %v", err)
	}
	stored, err := glStored(usage)
	if err != nil {
		t.Fatal(err)
	}
	effect := glMap(stored["reviewRevisionEffect"])
	if effect == nil {
		t.Fatal("the attempt carries no reviewRevisionEffect")
	}
	if len(effect) != 7 {
		t.Fatalf("effect keys: %v", effect)
	}
	effectID, idOK := rrInteger(effect["id"], 1)
	effectIndex, indexOK := rrInteger(effect["index"], 1)
	reportVersion, versionOK := rrInteger(effect["reportVersion"], 0)
	if !idOK || !indexOK || !versionOK || effectID != receipt.ID || effectIndex != receipt.Index {
		t.Fatalf("effect identity: %v", effect)
	}
	if glText(effect["kind"]) != "review" || glText(effect["contentHash"]) != receipt.ContentHash ||
		reportVersion != 2 || glText(effect["contextHash"]) != fixture.frozen.ContextHash {
		t.Fatalf("effect: %v", effect)
	}
	artifact := glMap(effect["artifact"])
	artifactRevision, artifactOK := rrInteger(glField(artifact, "revision"), 0)
	if artifact == nil || len(artifact) != 3 || !artifactOK || artifactRevision != 0 ||
		glText(glField(artifact, "artifactId")) != rccTestArtifactID ||
		glText(glField(artifact, "textHash")) != rrctxSHA(rccTestArtifact) {
		t.Fatalf("effect artifact: %v", effect["artifact"])
	}

	// A replay answers the same receipt and writes nothing: the file is what the
	// first commit left, and the tables hold one row each.
	before := rrctxDigest(t, fixture.path)
	replayed, err := fixture.commit(fixture.request)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed == nil || replayed.ID != receipt.ID || replayed.ContentHash != receipt.ContentHash ||
		replayed.Content != receipt.Content {
		t.Fatalf("replay receipt: %+v", replayed)
	}
	if after := rrctxDigest(t, fixture.path); after != before {
		t.Fatal("a replayed commit changed the database")
	}
	one := [3]int64{1, 1, 1}
	if counts := rccCounts(t, rccReadOnly(t, fixture.path)); counts != one {
		t.Fatalf("row counts after replay: %v", counts)
	}
	// The same run, presented for a different artifact, is a conflict rather than a
	// second review.
	conflict := fixture.request
	conflict.Artifact.TextHash = rrctxSHA("another answer")
	fixture.refused("different artifact after a saved review", conflict, ErrReviewCommitConflict)
}

// TestReviewCommitReviewRefusalsWriteNothing is the refusal half: each arm is a
// state the reference also refuses, and none of them may write a row or a page.
func TestReviewCommitReviewRefusalsWriteNothing(t *testing.T) {
	t.Run("stale context", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		// The author saved a new version of the chapter after the context was frozen.
		mustExec(t, fixture.db, "UPDATE contents SET body=? WHERE id=1", rrctxChapterOne+"灯下无人。")
		fixture.refused("reprooved context", fixture.request, ErrReviewCommitContextChanged)
	})
	t.Run("source status moved", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		mustExec(t, fixture.db, "UPDATE drafts SET status='finalized' WHERE id=1")
		fixture.refused("finalized source", fixture.request, ErrReviewCommitContextChanged)
	})
	t.Run("draft row gone", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		mustExec(t, fixture.db, "DELETE FROM drafts WHERE id=1")
		fixture.refused("missing source", fixture.request, ErrReviewCommitContextChanged)
	})
	t.Run("unknown handle", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.AssertPrepared = func(operation, text string) error {
			return ErrReviewCommitContextRequired
		}
		fixture.refused("handle this process never minted", request, ErrReviewCommitContextRequired)
	})
	t.Run("assertion sees the run identity", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		var operation, text string
		request := fixture.request
		request.AssertPrepared = func(gotOperation, gotText string) error {
			operation, text = gotOperation, gotText
			return nil
		}
		before := rrctxDigest(t, fixture.path)
		if _, err := fixture.commit(request); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// The proof is handed what the run itself froze, not what the caller claims:
		// the operation the binding carries and the exact bytes the manifest stored.
		if operation != ReviewRevisionOperationReviewChapter || text != fixture.frozen.Text {
			t.Fatalf("assertion saw operation %v over %v bytes", operation, len(text))
		}
		if after := rrctxDigest(t, fixture.path); after == before {
			t.Fatal("a successful commit left the database unchanged")
		}
	})
	t.Run("artifact not the settled receipt", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Artifact.TextHash = rrctxSHA("an answer the run never settled")
		fixture.refused("stale artifact", request, ErrReviewCommitArtifact)
	})
	t.Run("artifact revision moved", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Artifact.Revision = 1
		fixture.refused("wrong artifact revision", request, ErrReviewCommitArtifact)
	})
	t.Run("unknown artifact", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Artifact.ArtifactID = "artifact-never-written"
		fixture.refused("unknown artifact", request, ErrReviewCommitArtifact)
	})
	t.Run("run identity", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Handle.RunID = "run-never-opened"
		fixture.refused("unknown run", request, ErrReviewCommitIdentity)
	})
	t.Run("epoch moved", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.LiveEpoch = "epoch-after-the-lease-moved"
		fixture.refused("stale lease epoch", request, ErrReviewCommitContextChanged)
	})
	t.Run("cancelled root", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		action := plotObject{
			{"rootActionId", rccTestRootID},
			{"projectId", rrctxTestProjectID},
			{"epoch", rccTestEpoch},
			{"operation", ReviewRevisionOperationReviewChapter},
			{"status", "cancelled"},
		}
		mustExec(t, fixture.db, "UPDATE generation_roots SET action_json=? WHERE root_action_id=?",
			rrctxJSON(t, action), rccTestRootID)
		fixture.refused("cancelled action", fixture.request, ErrReviewCommitCancelled)
	})
	t.Run("frozen operation is not a review", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		// A refine run over the same chapter: the commit belongs to the other name.
		binding := plotObject{
			{"projectId", rrctxTestProjectID},
			{"epoch", rccTestEpoch},
			{"sourceManifest", plotObject{
				{"operation", ReviewRevisionOperationRefineDraft},
			}},
		}
		mustExec(t, fixture.db, "UPDATE generation_runs SET binding_json=? WHERE run_id=?",
			rrctxJSON(t, binding), rccTestRunID)
		fixture.refused("refine run", fixture.request, ErrReviewCommitContextUnproven)
	})
	t.Run("request without a project", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.ProjectID = ""
		fixture.refused("empty project", request, ErrReviewCommitInvalid)
	})
	t.Run("request without an artifact", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Artifact.ArtifactID = ""
		fixture.refused("empty artifact", request, ErrReviewCommitInvalid)
	})
	t.Run("request without a context hash", func(t *testing.T) {
		fixture := rccCommitFixture(t)
		request := fixture.request
		request.Artifact.TextHash = "NOT-A-DIGEST"
		fixture.refused("malformed digest", request, ErrReviewCommitInvalid)
	})
}
