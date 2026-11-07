//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the two names production_review_revision_commit_linux.go binds,
// over the real HTTP handler and the real frozen session:
//
//	review-revision:commit-revision   the pending revision, its content and the
//	                                  effect stamped on the settled attempt, in
//	                                  one transaction, answering the reference's
//	                                  receipt;
//	review-revision:read-recovery     the run's frozen context, its receipt, its
//	                                  composition and its latest candidate, read
//	                                  from one snapshot and answering the
//	                                  reference's recovery shape.
//
// The fixture is the editor-read root - a real v7 baseline with one project, one
// chapter and one working draft - plus two seeded review cycles and two seeded
// revisions. Those two rows make ordering and scoping real statements: a revision
// written by this lane must land at index 3, not at 1, and neither cycle may gain
// a revision_id, because this lane refuses to freeze a confirmation and therefore
// cannot prove a cycle binding.
//
// The lane is assembled here rather than by production_open_linux.go, because that
// file is frozen for this batch: the same durable bridge, the same transport
// server type and the same binders are installed, plus the two names under test.
// Every refusal is measured with productionDBMaintenanceDigest over the database
// file and its WAL sidecar, so a refusal that wrote one byte fails.
const (
	// productionReviewRevisionLaneNonce names the one dispatch this file makes
	// per lane.
	productionReviewRevisionLaneNonce = "review-revision-commit-1"
	// productionReviewRevisionLaneBeginNonce names the one admitted run per lane.
	productionReviewRevisionLaneBeginNonce = "review-revision-revision-begin-1"
	// productionReviewRevisionLaneBody is the prose the dispatch seam settles. It has
	// no leading or trailing whitespace, so the composition the lane stores is the
	// text itself, and it is materially longer than the fixture's source.
	productionReviewRevisionLaneBody = "第一章原稿：他推开门，看见走廊尽头亮着灯。\n风从窗缝里灌进来。"
	// productionReviewRevisionLanePrompt is the author guidance the frozen context
	// carries; it is the user_prompt the revision row must store.
	productionReviewRevisionLanePrompt = "把开篇写得更紧凑"
	// productionReviewRevisionLaneForeignProject is a project id this lane never
	// opened. A handle that names it is a request about another database.
	productionReviewRevisionLaneForeignProject = "proj-0000-review-revision-foreign"
	// productionReviewRevisionLaneFindingHash is a well-formed finding-set hash
	// for the two seeded cycles; this lane never reads it, and a value that is not
	// a digest would make the seed fail rather than the test.
	productionReviewRevisionLaneFindingHash = "1111111111111111111111111111111111111111111111111111111111111111"
)

// productionReviewRevisionLane is one disposable admitted project, the
// transport server this file assembled, and the author session over it.
type productionReviewRevisionLane struct {
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
	seam        *productionGenerationSeam
}

// productionReviewRevisionLaneStale is the three-key context a server that
// never issued this lease would have: the same project, a lease it cannot prove.
func productionReviewRevisionLaneStale(lane *productionReviewRevisionLane) map[string]any {
	return map[string]any{
		"projectId":   lane.frozen["projectId"],
		"projectPath": lane.projectPath,
		"leaseId":     "stale-lease",
	}
}

// productionReviewRevisionLaneHash is textHash (generation-run-repository.ts:92)
// over a Go string: the SHA-256 of its UTF-8 bytes, lower-case hex.
func productionReviewRevisionLaneHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// productionReviewRevisionLaneSeed writes the two revisions and the two review
// cycles the ordering and scoping assertions rest on, and optionally the trigger
// that makes the second write of a commit fail. It runs before the lane opens: the
// admitted handle is pinned to the file's content identity, so a write after the
// borrow fence would refuse every later call.
func productionReviewRevisionLaneSeed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	seed.SetMaxOpenConns(1)
	sourceHash := productionReviewRevisionLaneHash(productionEditorSeed)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO contents (id, body) VALUES (2, ?)`, []any{"第一轮审稿：开篇略松。"}},
		{`INSERT INTO contents (id, body) VALUES (3, ?)`, []any{"第二轮审稿：门后无人。"}},
		{`INSERT INTO contents (id, body) VALUES (4, ?)`, []any{"旧修稿一"}},
		{`INSERT INTO contents (id, body) VALUES (5, ?)`, []any{"旧修稿二"}},
		{`INSERT INTO reviews (id,base_draft_id,review_index,source_draft_chapter_number,
 source_draft_version,source_draft_status,source_content,content_id)
 VALUES (1,1,1,1,1,'draft',?,2)`, []any{productionEditorSeed}},
		{`INSERT INTO reviews (id,base_draft_id,review_index,source_draft_chapter_number,
 source_draft_version,source_draft_status,source_content,content_id)
 VALUES (2,1,2,1,1,'draft',?,3)`, []any{productionEditorSeed}},
		{`INSERT INTO revisions (id,base_draft_id,revision_index,revision_type,status,user_prompt,
 source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id,word_count)
 VALUES (1,1,1,'refine','discarded','',1,1,'draft',?,4,5)`, []any{productionEditorSeed}},
		{`INSERT INTO revisions (id,base_draft_id,revision_index,revision_type,status,user_prompt,
 source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id,word_count)
 VALUES (2,1,2,'refine','merged','',1,1,'draft',?,5,5)`, []any{productionEditorSeed}},
		{`INSERT INTO generation_roots (root_action_id,idempotency_key,action_json,budget_json)
 VALUES (?,?,?,'{}')`, []any{"root-cycle-1", "idem-cycle-1",
			`{"rootActionId":"root-cycle-1","projectId":"` + productionEditorProjectID +
				`","epoch":"lease-history","operation":"review-chapter","status":"committed"}`}},
		{`INSERT INTO generation_roots (root_action_id,idempotency_key,action_json,budget_json)
 VALUES (?,?,?,'{}')`, []any{"root-cycle-2", "idem-cycle-2",
			`{"rootActionId":"root-cycle-2","projectId":"` + productionEditorProjectID +
				`","epoch":"lease-history","operation":"review-chapter","status":"committed"}`}},
		{`INSERT INTO review_cycles (cycle_id,root_action_id,review_id,review_content_hash,
 source_hash,finding_set_hash,comparison_version,revision_status,recheck_count)
 VALUES (?,?,?,?,?,?,1,'not-generated',0)`, []any{"cycle-1", "root-cycle-1", 1,
			productionReviewRevisionLaneFindingHash, sourceHash, productionReviewRevisionLaneFindingHash}},
		{`INSERT INTO review_cycles (cycle_id,root_action_id,review_id,review_content_hash,
 source_hash,finding_set_hash,comparison_version,revision_status,recheck_count)
 VALUES (?,?,?,?,?,?,1,'not-generated',0)`, []any{"cycle-2", "root-cycle-2", 2,
			productionReviewRevisionLaneFindingHash, sourceHash, productionReviewRevisionLaneFindingHash}},
	}
	productionDBMaintenanceExec(t, seed, statements)
	productionDBMaintenanceCheckpoint(t, seed, database)
}

// productionReviewRevisionLaneOpen builds the fixture, seeds it, opens the
// production lane and mounts the binders these two names need on one transport
// server. The dispatch seam replaces the model call; nothing dials.
func productionReviewRevisionLaneOpen(t *testing.T) *productionReviewRevisionLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	_, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	productionReviewRevisionLaneSeed(t, projectPath)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	seam := &productionGenerationSeam{text: productionReviewRevisionLaneBody}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-review-revision-commit")
	owner, err := newProductionGeneration(open, server, seam.dispatch)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation admission is unavailable: %v", err)
	}
	for _, bind := range []struct {
		label string
		bind  func() error
	}{
		{"project:open", func() error { return server.BindProjects(open.durable) }},
		{"generation:begin", func() error {
			return server.BindGenerationBegin(&transport.GenerationProvider{Begin: owner.Begin})
		}},
		{"generation:execute", func() error {
			return server.BindGenerationExecute(&transport.GenerationProvider{Execute: owner.Execute})
		}},
		{"generation:compose-visible", func() error { return newProductionComposition(owner).Bind(server) }},
		{"review-revision:prepare", func() error { return newProductionReviewRevisions(open.durable).Bind(server) }},
		{"review-revision:commit-revision", func() error {
			return newProductionReviewRevisionCommits(open.durable).Bind(server)
		}},
	} {
		if err := bind.bind(); err != nil {
			_ = open.Close()
			t.Fatalf("%s: %v", bind.label, err)
		}
	}
	t.Cleanup(func() { _ = open.Close() })
	author, frozen := productionExecuteSession(t, server, projectPath, productionReviewRevisionLaneNonce)
	return &productionReviewRevisionLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen:      frozen,
		seam:        seam,
	}
}

// productionReviewRevisionLanePrepare freezes one refine-draft context through
// the served prepare and returns its handle and the context the browser holds.
func productionReviewRevisionLanePrepare(t *testing.T, lane *productionReviewRevisionLane) (string, map[string]any) {
	t.Helper()
	request := map[string]any{
		"operation": store.ReviewRevisionOperationRefineDraft,
		"draftId":   int64(1),
		"expectedDraft": map[string]any{
			"chapterNumber": int64(1),
			"version":       int64(1),
			"status":        "draft",
			"contentHash":   productionReviewRevisionLaneHash(productionEditorSeed),
		},
		"authorInputs": []any{map[string]any{"id": "user-prompt", "text": productionReviewRevisionLanePrompt}},
		"uiLocale":     store.ReviewRevisionLocaleZH,
	}
	var answered struct {
		ContextID string         `json:"contextId"`
		Context   map[string]any `json:"context"`
	}
	productionEditorDecode(t, lane.author, productionReviewRevisionPrepare,
		[]any{request, lane.frozen}, &answered)
	if answered.ContextID == "" || answered.Context == nil {
		t.Fatalf("%s answered no prepared context: %v %v", productionReviewRevisionPrepare,
			answered.ContextID, answered.Context)
	}
	if syntheticString(answered.Context["operation"]) != store.ReviewRevisionOperationRefineDraft {
		t.Fatalf("the prepared context names another operation: %v", answered.Context["operation"])
	}
	return answered.ContextID, answered.Context
}

// productionReviewRevisionLaneBegin opens one admitted run over the prepared
// context and returns its handle.
func productionReviewRevisionLaneBegin(t *testing.T, lane *productionReviewRevisionLane,
	contextID string, contextValue map[string]any, nonce string) map[string]any {
	t.Helper()
	canonical, err := json.Marshal(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	selection := map[string]any{
		"operation":                       store.ReviewRevisionOperationRefineDraft,
		"uiActionNonce":                   nonce,
		"modelId":                         productionTestModelID,
		"chapterNumber":                   int64(1),
		"selectedDraftIds":                []any{int64(1)},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{"refine_chapter"},
		"skillStages":                     []any{"refinement"},
		"authorInputs": []any{map[string]any{
			"id": productionReviewRevisionAuthorInputID, "text": string(canonical),
		}},
		"output":                  "visible-text",
		"reviewRevisionContextId": contextID,
	}
	result := productionStoryStateWriteCall(t, lane.author, "generation:begin", []any{selection, lane.frozen})
	handle, ok := result["handle"].(map[string]any)
	if !ok || syntheticString(handle["runId"]) == "" || syntheticString(handle["rootActionId"]) == "" {
		t.Fatalf("generation:begin returned no run handle: %v", result)
	}
	return handle
}

// productionReviewRevisionLaneSettledArtifact reads the run's settled artifact id back
// over a connection of the test's own.
func productionReviewRevisionLaneSettledArtifact(t *testing.T, projectPath, runID string) string {
	t.Helper()
	var artifactID string
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, projectPath),
		"SELECT artifact_id FROM generation_artifacts WHERE run_id=? ORDER BY rowid LIMIT 1",
		[]any{runID}, &artifactID)
	if artifactID == "" {
		t.Fatalf("the executed run wrote no artifact row")
	}
	return artifactID
}

// productionReviewRevisionLaneRun is one prepared run, the handle it opened, and
// the composition the run stored.
type productionReviewRevisionLaneRun struct {
	ContextID       string
	Context         map[string]any
	Handle          map[string]any
	ArtifactID      string
	CompositionHash string
}

// productionReviewRevisionLaneCompose settles one artifact through the served
// execute, then stores the visible composition through the served
// generation:compose-visible - the two writes the renderer performs before it
// commits a revision.
func productionReviewRevisionLaneCompose(t *testing.T, lane *productionReviewRevisionLane,
	run *productionReviewRevisionLaneRun, nonce string) {
	t.Helper()
	runID := syntheticString(run.Handle["runId"])
	executed := productionExecuteAnswer(t, lane.author,
		productionExecuteRequest(run.Handle, nonce), lane.frozen)
	outcome := syntheticMap(executed["outcome"])
	if syntheticString(outcome["finishReason"]) != "stop" ||
		syntheticString(outcome["content"]) != productionReviewRevisionLaneBody {
		t.Fatalf("the run did not settle the text it dispatched: %v", outcome)
	}
	if calls := lane.seam.dispatched(); calls != 1 {
		t.Fatalf("generation:execute dispatched %v times; want one", calls)
	}
	run.ArtifactID = productionReviewRevisionLaneSettledArtifact(t, lane.projectPath, runID)
	run.CompositionHash = productionReviewRevisionLaneHash(productionReviewRevisionLaneBody)
	var receipt struct {
		Algorithm   string   `json:"algorithm"`
		Text        string   `json:"text"`
		TextHash    string   `json:"textHash"`
		ArtifactIDs []string `json:"artifactIds"`
	}
	productionEditorDecode(t, lane.author, "generation:compose-visible",
		[]any{run.Handle, []any{run.ArtifactID}, run.CompositionHash, lane.frozen}, &receipt)
	if receipt.Algorithm != draftalg.VisibleContinuationVersion || receipt.Text != productionReviewRevisionLaneBody ||
		receipt.TextHash != run.CompositionHash || len(receipt.ArtifactIDs) != 1 ||
		receipt.ArtifactIDs[0] != run.ArtifactID {
		t.Fatalf("the stored composition is not the text the run settled: %+v", receipt)
	}
}

// productionReviewRevisionLanePrepareRun prepares and begins one run and, when
// compose is set, settles and composes its artifact.
func productionReviewRevisionLanePrepareRun(t *testing.T, lane *productionReviewRevisionLane,
	nonce string, compose bool) *productionReviewRevisionLaneRun {
	t.Helper()
	run := &productionReviewRevisionLaneRun{}
	run.ContextID, run.Context = productionReviewRevisionLanePrepare(t, lane)
	run.Handle = productionReviewRevisionLaneBegin(t, lane, run.ContextID, run.Context, nonce)
	if compose {
		productionReviewRevisionLaneCompose(t, lane, run, nonce+"-execute")
	}
	return run
}

// productionReviewRevisionLaneWire is the renderer's commit request
// (review-revision-command.ts:172-174): three keys, nothing else.
func productionReviewRevisionLaneWire(run *productionReviewRevisionLaneRun) map[string]any {
	return map[string]any{
		"contextId": run.ContextID,
		"handle": map[string]any{
			"projectId":    run.Handle["projectId"],
			"epoch":        run.Handle["epoch"],
			"rootActionId": run.Handle["rootActionId"],
			"runId":        run.Handle["runId"],
		},
		"expectedCompositionHash": run.CompositionHash,
	}
}

// productionReviewRevisionLaneReceipt is ReviewRevisionCommitReceipt as the
// browser decodes it.
type productionReviewRevisionLaneReceipt struct {
	Success        bool   `json:"success"`
	Kind           string `json:"kind"`
	ID             int64  `json:"id"`
	Index          int64  `json:"index"`
	Content        string `json:"content"`
	ContentHash    string `json:"contentHash"`
	RevisionStatus string `json:"revisionStatus"`
	Source         struct {
		ID            int64  `json:"id"`
		ChapterNumber int64  `json:"chapterNumber"`
		Version       int64  `json:"version"`
		Status        string `json:"status"`
		Content       string `json:"content"`
	} `json:"source"`
}

// productionReviewRevisionLaneRowCounts is a read-only count of the rows a commit
// writes or sweeps, so a refusal can be proved to have written nothing even when
// the transaction it never reached would have rolled back.
// productionReviewRevisionLaneDigest is the whole-file identity of the seeded
// database, for asserting that a replayed write changed nothing.
func productionReviewRevisionLaneDigest(t *testing.T, projectPath string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func productionReviewRevisionLaneRowCounts(t *testing.T, projectPath string) [3]int64 {
	t.Helper()
	readback := productionStoryStateWriteReadback(t, projectPath)
	var counts [3]int64
	for index, query := range []string{
		"SELECT COUNT(*) FROM revisions",
		"SELECT COUNT(*) FROM contents",
		"SELECT COUNT(*) FROM review_cycles WHERE revision_id IS NOT NULL",
	} {
		productionStoryStateWriteScan(t, readback, query, nil, &counts[index])
	}
	return counts
}

// productionReviewRevisionLaneRefused asserts one refusal: not a 200, not the
// lane's fail-closed 501, a symbolic code, the same row counts and the same
// database digest afterwards.
func productionReviewRevisionLaneRefused(t *testing.T, lane *productionReviewRevisionLane,
	operation, label string, args []any, counts [3]int64, digest string, want ...string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %s is still the lane's fail-closed 501: %s", label, operation, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 for a refused call: %s", label, operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if !productionReviewRevisionCommitSymbolic(body.Error.Code) {
		t.Fatalf("%s: refused with %v, which is not a symbolic code: %s", label, body.Error.Code,
			response.Body.String())
	}
	matched := len(want) == 0
	for _, code := range want {
		if body.Error.Code == code {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("%s: refused with %v; want one of %v: %s", label, body.Error.Code, want,
			response.Body.String())
	}
	if after := productionReviewRevisionLaneRowCounts(t, lane.projectPath); after != counts {
		t.Fatalf("%s: a refused call wrote rows: %v -> %v", label, counts, after)
	}
	if after := productionDBMaintenanceDigest(t, lane.database); after != digest {
		t.Fatalf("%s: a refused call changed the admitted author database", label)
	}
}

// TestProductionReviewRevisionLaneCommitRevision is the write half: the served
// prepare, begin, execute and compose, then the commit itself.
func TestProductionReviewRevisionLaneCommitRevision(t *testing.T) {
	t.Run("review-revision:commit-revision", testProductionReviewRevisionCommitRevision)
}

func testProductionReviewRevisionCommitRevision(t *testing.T) {
	lane := productionReviewRevisionLaneOpen(t)
	run := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce, true)
	wire := productionReviewRevisionLaneWire(run)

	var receipt productionReviewRevisionLaneReceipt
	productionEditorDecode(t, lane.author, productionReviewRevisionCommitRevisionName,
		[]any{wire, lane.frozen}, &receipt)
	if !receipt.Success || receipt.Kind != "revision" || receipt.ID < 1 {
		t.Fatalf("the commit answered %+v; want a saved revision receipt", receipt)
	}
	// The ordering statement: two revisions already exist for this draft, so the
	// new one is r3 and the reference's MAX(revision_index)+1 is what produced it.
	if receipt.Index != 3 {
		t.Fatalf("the committed revision is r%d; want r3 after two seeded revisions", receipt.Index)
	}
	if receipt.Content != productionReviewRevisionLaneBody ||
		receipt.ContentHash != productionReviewRevisionLaneHash(receipt.Content) {
		t.Fatalf("the receipt's content is not the composed text or its hash is wrong: %+v", receipt)
	}
	if receipt.RevisionStatus != "pending" {
		t.Fatalf("the committed revision reports status %v; want pending", receipt.RevisionStatus)
	}
	if receipt.Source.ID != 1 || receipt.Source.ChapterNumber != 1 || receipt.Source.Version != 1 ||
		receipt.Source.Status != "draft" || receipt.Source.Content != productionEditorSeed {
		t.Fatalf("the receipt names another source draft: %+v", receipt.Source)
	}

	// The rows, read back over a connection of the test's own.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var baseID, index, chapter, version, wordCount, contentID int64
	var kind, status, prompt, sourceStatus, sourceContent string
	productionStoryStateWriteScan(t, readback,
		`SELECT base_draft_id,revision_index,revision_type,status,user_prompt,
 source_draft_chapter_number,source_draft_version,source_draft_status,source_content,word_count,content_id
 FROM revisions WHERE id=?`, []any{receipt.ID},
		&baseID, &index, &kind, &status, &prompt, &chapter, &version, &sourceStatus,
		&sourceContent, &wordCount, &contentID)
	if baseID != 1 || index != receipt.Index || kind != "refine" || status != "pending" ||
		prompt != productionReviewRevisionLanePrompt || chapter != 1 || version != 1 ||
		sourceStatus != "draft" || sourceContent != productionEditorSeed {
		t.Fatalf("the committed revision reads back as draft %v r%v %v %v prompt %v source ch%v v%v %v",
			baseID, index, kind, status, prompt, chapter, version, sourceStatus)
	}
	// countDraftUnits is the reference's persisted unit, not the character count.
	if want := int64(draftalg.CountDraftUnits(productionReviewRevisionLaneBody)); wordCount != want {
		t.Fatalf("the committed revision stores %v units; the reference counts %v", wordCount, want)
	}
	var body string
	productionStoryStateWriteScan(t, readback, "SELECT body FROM contents WHERE id=?", []any{contentID}, &body)
	if body != receipt.Content || productionReviewRevisionLaneHash(body) != receipt.ContentHash {
		t.Fatalf("the committed content row is not the receipt's content")
	}
	// The effect that makes this run's revision spent: the settled attempt carries
	// it, with the composition the receipt was proved against.
	var effectKind, effectHash string
	var effectID int64
	productionStoryStateWriteScan(t, readback,
		`SELECT json_extract(usage_receipt_json,'$.reviewRevisionEffect.kind'),
 json_extract(usage_receipt_json,'$.reviewRevisionEffect.compositionHash'),
 json_extract(usage_receipt_json,'$.reviewRevisionEffect.id')
 FROM generation_attempts WHERE run_id=?`, []any{syntheticString(run.Handle["runId"])},
		&effectKind, &effectHash, &effectID)
	if effectKind != "revision" || effectHash != run.CompositionHash || effectID != receipt.ID {
		t.Fatalf("the settled attempt carries effect %v hash %v id %v", effectKind, effectHash, effectID)
	}
	// The cycle scoping statement: two cycles exist, and neither gained a revision.
	if counts := productionReviewRevisionLaneRowCounts(t, lane.projectPath); counts[2] != 0 {
		t.Fatalf("%v review cycles claim a revision; this lane cannot prove a cycle binding", counts[2])
	}

	// The replay: the reference answers the saved receipt instead of writing twice.
	digest := productionDBMaintenanceDigest(t, lane.database)
	counts := productionReviewRevisionLaneRowCounts(t, lane.projectPath)
	var again productionReviewRevisionLaneReceipt
	productionEditorDecode(t, lane.author, productionReviewRevisionCommitRevisionName,
		[]any{productionReviewRevisionLaneWire(run), lane.frozen}, &again)
	if !again.Success || again.ID != receipt.ID || again.Index != receipt.Index ||
		again.ContentHash != receipt.ContentHash {
		t.Fatalf("the replay answered another receipt: %+v", again)
	}
	if after := productionReviewRevisionLaneRowCounts(t, lane.projectPath); after != counts {
		t.Fatalf("the replay wrote rows: %v -> %v", counts, after)
	}
	if after := productionDBMaintenanceDigest(t, lane.database); after != digest {
		t.Fatalf("the replay changed the admitted author database")
	}

	// The spent arm: the same run, already spent, presented for another
	// composition. This is the conflict the reference raises.
	spent := productionReviewRevisionLaneWire(run)
	spent["expectedCompositionHash"] = productionReviewRevisionLaneHash("another composition")
	productionReviewRevisionLaneRefused(t, lane, productionReviewRevisionCommitRevisionName,
		"a spent composition", []any{spent, lane.frozen}, counts, digest,
		"GENERATION_REVIEW_COMMIT_CONFLICT")
}

// TestProductionReviewRevisionLaneReadRecovery is the read half, and the scoping
// statement: the receipt a run saved belongs to that run and to no other.
func TestProductionReviewRevisionLaneReadRecovery(t *testing.T) {
	t.Run("review-revision:read-recovery", testProductionReviewRevisionReadRecovery)
}

// productionReviewRevisionLaneRecovery is ReviewRevisionRecovery as the browser
// decodes it.
type productionReviewRevisionLaneRecovery struct {
	Handle struct {
		ProjectID    string `json:"projectId"`
		Epoch        string `json:"epoch"`
		RootActionID string `json:"rootActionId"`
		RunID        string `json:"runId"`
	} `json:"handle"`
	Context      map[string]any `json:"context"`
	ModelID      string         `json:"modelId"`
	SourceStatus string         `json:"sourceStatus"`
	InFlight     bool           `json:"inFlight"`
	CanResume    bool           `json:"canResume"`
	ContextID    string         `json:"contextId"`
	Saved        *struct {
		Success        bool   `json:"success"`
		Kind           string `json:"kind"`
		ID             int64  `json:"id"`
		Index          int64  `json:"index"`
		Content        string `json:"content"`
		ContentHash    string `json:"contentHash"`
		RevisionStatus string `json:"revisionStatus"`
	} `json:"saved"`
	Composition *struct {
		Algorithm   string   `json:"algorithm"`
		TextHash    string   `json:"textHash"`
		ArtifactIDs []string `json:"artifactIds"`
	} `json:"composition"`
	LastCompositionFinishReason *string `json:"lastCompositionFinishReason"`
	LatestArtifact              *struct {
		ArtifactID string `json:"artifactId"`
		AttemptID  string `json:"attemptId"`
		Text       string `json:"text"`
		Status     string `json:"status"`
	} `json:"latestArtifact"`
	LatestArtifactFinishReason *string  `json:"latestArtifactFinishReason"`
	AttemptedPurposes          []string `json:"attemptedPurposes"`
}

func testProductionReviewRevisionReadRecovery(t *testing.T) {
	lane := productionReviewRevisionLaneOpen(t)
	run := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce, true)
	var receipt productionReviewRevisionLaneReceipt
	productionEditorDecode(t, lane.author, productionReviewRevisionCommitRevisionName,
		[]any{productionReviewRevisionLaneWire(run), lane.frozen}, &receipt)

	// A second run over the same draft that never committed anything: its recovery
	// must not answer the first run's receipt.
	other := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce+"-2", false)

	var recovered productionReviewRevisionLaneRecovery
	productionEditorDecode(t, lane.author, productionReviewRevisionReadRecoveryName,
		[]any{map[string]any{"handle": run.Handle}, lane.frozen}, &recovered)
	if recovered.Handle.RunID != syntheticString(run.Handle["runId"]) ||
		recovered.Handle.RootActionID != syntheticString(run.Handle["rootActionId"]) ||
		recovered.Handle.ProjectID != syntheticString(run.Handle["projectId"]) ||
		recovered.Handle.Epoch != syntheticString(run.Handle["epoch"]) {
		t.Fatalf("the recovery echoes another handle: %+v", recovered.Handle)
	}
	// The frozen context travels as the run stored it: the same operation, source
	// and author input the prepare answered.
	if syntheticString(recovered.Context["operation"]) != store.ReviewRevisionOperationRefineDraft ||
		recovered.Context["sourceHash"] != productionReviewRevisionLaneHash(productionEditorSeed) {
		t.Fatalf("the recovery carries another frozen context: %v", recovered.Context)
	}
	if recovered.ModelID != productionTestModelID {
		t.Fatalf("the recovery names model %v", recovered.ModelID)
	}
	// The one field this lane cannot prove: the reference's own fallback, with the
	// reference's own consequence for canResume.
	if recovered.SourceStatus != store.ReviewRevisionSourceConflict || recovered.CanResume {
		t.Fatalf("the recovery claims sourceStatus %v and canResume %v", recovered.SourceStatus,
			recovered.CanResume)
	}
	if recovered.InFlight {
		t.Fatalf("a settled run reports an in-flight dispatch")
	}
	if recovered.ContextID != "" {
		t.Fatalf("the recovery minted contextId %v without a resumable source", recovered.ContextID)
	}
	// The receipt, proved against the rows the commit wrote.
	if recovered.Saved == nil || !recovered.Saved.Success || recovered.Saved.Kind != "revision" ||
		recovered.Saved.ID != receipt.ID || recovered.Saved.Index != receipt.Index ||
		recovered.Saved.ContentHash != receipt.ContentHash ||
		recovered.Saved.RevisionStatus != "pending" ||
		recovered.Saved.Content != productionReviewRevisionLaneBody {
		t.Fatalf("the recovery's saved receipt is not the commit's: %+v", recovered.Saved)
	}
	if recovered.Composition == nil || recovered.Composition.TextHash != run.CompositionHash ||
		recovered.Composition.Algorithm != draftalg.VisibleContinuationVersion ||
		len(recovered.Composition.ArtifactIDs) != 1 ||
		recovered.Composition.ArtifactIDs[0] != run.ArtifactID {
		t.Fatalf("the recovery's composition is not the run's: %+v", recovered.Composition)
	}
	if recovered.LastCompositionFinishReason == nil || *recovered.LastCompositionFinishReason != "stop" {
		t.Fatalf("the recovery's composition finish reason is %v", recovered.LastCompositionFinishReason)
	}
	if recovered.LatestArtifact == nil || recovered.LatestArtifact.ArtifactID != run.ArtifactID ||
		recovered.LatestArtifact.Text != productionReviewRevisionLaneBody ||
		recovered.LatestArtifact.Status != "completed" {
		t.Fatalf("the recovery's latest artifact is not the settled one: %+v", recovered.LatestArtifact)
	}
	if recovered.LatestArtifactFinishReason == nil || *recovered.LatestArtifactFinishReason != "stop" {
		t.Fatalf("the recovery's artifact finish reason is %v", recovered.LatestArtifactFinishReason)
	}
	if len(recovered.AttemptedPurposes) != 1 || recovered.AttemptedPurposes[0] != "chapter-draft" {
		t.Fatalf("the recovery reports attempts %v", recovered.AttemptedPurposes)
	}

	// The second run: no receipt, no composition, no artifact, and the reference's
	// nulls rather than a fabricated answer.
	var empty productionReviewRevisionLaneRecovery
	productionEditorDecode(t, lane.author, productionReviewRevisionReadRecoveryName,
		[]any{map[string]any{"handle": other.Handle}, lane.frozen}, &empty)
	if empty.Handle.RunID != syntheticString(other.Handle["runId"]) {
		t.Fatalf("the second run's recovery echoes run %v", empty.Handle.RunID)
	}
	if empty.Saved != nil || empty.Composition != nil || empty.LatestArtifact != nil {
		t.Fatalf("the second run's recovery borrowed the first run's state: %+v", empty)
	}
	if empty.LastCompositionFinishReason != nil || empty.LatestArtifactFinishReason != nil {
		t.Fatalf("an unexecuted run reports finish reasons %v %v",
			empty.LastCompositionFinishReason, empty.LatestArtifactFinishReason)
	}
	if empty.AttemptedPurposes == nil || len(empty.AttemptedPurposes) != 0 {
		t.Fatalf("an unexecuted run reports attempts %v; want an empty array", empty.AttemptedPurposes)
	}
	if empty.SourceStatus != store.ReviewRevisionSourceConflict || empty.CanResume || empty.InFlight {
		t.Fatalf("the second run's recovery claims %v/%v/%v", empty.SourceStatus, empty.CanResume,
			empty.InFlight)
	}
}

// TestProductionReviewRevisionLaneRefusals fences both names: a foreign
// project, a stale lease, a malformed body, an unminted handle, a run this
// database does not hold and a composition that is not the run's.
func TestProductionReviewRevisionLaneRefusals(t *testing.T) {
	t.Run("review-revision:commit-revision", testProductionReviewRevisionCommitRefusals)
	t.Run("review-revision:read-recovery", testProductionReviewRevisionReadRecoveryRefusals)
}

func testProductionReviewRevisionCommitRefusals(t *testing.T) {
	lane := productionReviewRevisionLaneOpen(t)
	run := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce, true)
	counts := productionReviewRevisionLaneRowCounts(t, lane.projectPath)
	digest := productionDBMaintenanceDigest(t, lane.database)
	operation := productionReviewRevisionCommitRevisionName

	// A handle that names another project: the reference's requireRun compares it
	// with the run it is about to write, and the borrowed database is the lease's.
	foreign := productionReviewRevisionLaneWire(run)
	foreign["handle"].(map[string]any)["projectId"] = productionReviewRevisionLaneForeignProject
	productionReviewRevisionLaneRefused(t, lane, operation, "a foreign project",
		[]any{foreign, lane.frozen}, counts, digest, "GENERATION_RUN_IDENTITY_MISMATCH")
	// A lease this server never issued: the borrow refuses before the body is read.
	productionReviewRevisionLaneRefused(t, lane, operation, "a stale lease",
		[]any{productionReviewRevisionLaneWire(run), productionReviewRevisionLaneStale(lane)}, counts, digest,
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_ROOT_MISMATCH",
		"PROJECT_CONTEXT_CHANGED", "PROJECT_NOT_OPEN")
	// The binder's arity: one request plus the frozen context.
	productionReviewRevisionLaneRefused(t, lane, operation, "a short wire call",
		[]any{productionReviewRevisionLaneWire(run)}, counts, digest, "INVALID_ARGUMENTS")
	// A request shape this lane never agreed to serve.
	extra := productionReviewRevisionLaneWire(run)
	extra["extra"] = true
	productionReviewRevisionLaneRefused(t, lane, operation, "an extra request key",
		[]any{extra, lane.frozen}, counts, digest, "GENERATION_REVIEW_CONTEXT_INVALID")
	// A context id this process never minted: the handle proof runs before the
	// first write and the reference's own answer is "prepare again".
	unminted := productionReviewRevisionLaneWire(run)
	unminted["contextId"] = productionReviewRevisionAbsentHandle
	productionReviewRevisionLaneRefused(t, lane, operation, "an unminted handle",
		[]any{unminted, lane.frozen}, counts, digest, "GENERATION_REVIEW_CONTEXT_REQUIRED")
	// The composition hash the run did not store.
	wrong := productionReviewRevisionLaneWire(run)
	wrong["expectedCompositionHash"] = productionReviewRevisionLaneHash("not this run")
	productionReviewRevisionLaneRefused(t, lane, operation, "another composition",
		[]any{wrong, lane.frozen}, counts, digest, "GENERATION_REVIEW_COMPOSITION_REQUIRED")
}

func testProductionReviewRevisionReadRecoveryRefusals(t *testing.T) {
	lane := productionReviewRevisionLaneOpen(t)
	run := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce, true)
	counts := productionReviewRevisionLaneRowCounts(t, lane.projectPath)
	digest := productionDBMaintenanceDigest(t, lane.database)
	operation := productionReviewRevisionReadRecoveryName

	foreign := map[string]any{"handle": map[string]any{
		"projectId":    productionReviewRevisionLaneForeignProject,
		"epoch":        run.Handle["epoch"],
		"rootActionId": run.Handle["rootActionId"],
		"runId":        run.Handle["runId"],
	}}
	productionReviewRevisionLaneRefused(t, lane, operation, "a foreign project",
		[]any{foreign, lane.frozen}, counts, digest, "GENERATION_RUN_IDENTITY_MISMATCH")
	productionReviewRevisionLaneRefused(t, lane, operation, "a stale lease",
		[]any{map[string]any{"handle": run.Handle}, productionReviewRevisionLaneStale(lane)}, counts, digest,
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_ROOT_MISMATCH",
		"PROJECT_CONTEXT_CHANGED", "PROJECT_NOT_OPEN")
	productionReviewRevisionLaneRefused(t, lane, operation, "a short wire call",
		[]any{map[string]any{"handle": run.Handle}}, counts, digest, "INVALID_ARGUMENTS")
	// The request is exactly `{handle}`: the renderer sends nothing else, and a
	// body that names a project is a request this lane never agreed to serve.
	productionReviewRevisionLaneRefused(t, lane, operation, "an extra request key",
		[]any{map[string]any{"handle": run.Handle, "projectId": productionReviewRevisionLaneForeignProject},
			lane.frozen}, counts, digest, "GENERATION_REVIEW_CONTEXT_INVALID")
	// A run this database does not hold, and one frozen under another epoch.
	absent := map[string]any{"handle": map[string]any{
		"projectId":    run.Handle["projectId"],
		"epoch":        run.Handle["epoch"],
		"rootActionId": run.Handle["rootActionId"],
		"runId":        "run-absent",
	}}
	productionReviewRevisionLaneRefused(t, lane, operation, "a run this database does not hold",
		[]any{absent, lane.frozen}, counts, digest, "GENERATION_RUN_IDENTITY_MISMATCH")
	movedEpoch := map[string]any{"handle": map[string]any{
		"projectId":    run.Handle["projectId"],
		"epoch":        "lease-moved",
		"rootActionId": run.Handle["rootActionId"],
		"runId":        run.Handle["runId"],
	}}
	productionReviewRevisionLaneRefused(t, lane, operation, "a handle frozen under another epoch",
		[]any{movedEpoch, lane.frozen}, counts, digest, "GENERATION_RUN_IDENTITY_MISMATCH")
}

// TestProductionReviewRevisionLaneSecondWrite is the idempotence arm. A trigger
// cannot prove rollback here: admission fingerprints sqlite_master, so a trigger in
// the seeded database would make the project's schema unrecognizable before any
// handler ran. The operational fact a failed-write test would have proven - that the
// write is one transaction - is instead pinned at the store level by the single
// BEGIN IMMEDIATE, and the same NOT NULL rollback pattern is proven for the shared
// transaction helper in production_db_maintenance_linux_test.go. What this arm
// proves is the second fact a commit has to honour: replaying the same commit
// answers the same receipt and writes nothing new.
func TestProductionReviewRevisionLaneSecondWrite(t *testing.T) {
	t.Run("review-revision:commit-revision replays without a second write",
		testProductionReviewRevisionCommitReplay)
}

func testProductionReviewRevisionCommitReplay(t *testing.T) {
	lane := productionReviewRevisionLaneOpen(t)
	run := productionReviewRevisionLanePrepareRun(t, lane, productionReviewRevisionLaneBeginNonce, true)
	counts := productionReviewRevisionLaneRowCounts(t, lane.projectPath)
	if counts[0] != 2 {
		t.Fatalf("the fixture seeded %v revisions; want two", counts[0])
	}
	wire := productionReviewRevisionLaneWire(run)
	first := lane.author.call(t, productionReviewRevisionCommitRevisionName,
		[]any{wire, lane.frozen})
	if first.Code != http.StatusOK {
		t.Fatalf("the first commit answered HTTP %v: %s", first.Code, first.Body.String())
	}
	after := productionReviewRevisionLaneRowCounts(t, lane.projectPath)
	if after[0] != 3 || after[1] != counts[1]+1 {
		t.Fatalf("the first commit left %v revisions and %v contents; want 3 and %v",
			after[0], after[1], counts[1]+1)
	}
	digest := productionReviewRevisionLaneDigest(t, lane.projectPath)
	replay := lane.author.call(t, productionReviewRevisionCommitRevisionName,
		[]any{wire, lane.frozen})
	if replay.Code != http.StatusOK {
		t.Fatalf("the replay answered HTTP %v: %s", replay.Code, replay.Body.String())
	}
	if afterReplay := productionReviewRevisionLaneRowCounts(t, lane.projectPath); afterReplay != after {
		t.Fatalf("the replay wrote again: %v after %v", afterReplay, after)
	}
	if productionReviewRevisionLaneDigest(t, lane.projectPath) != digest {
		t.Fatal("a replayed commit changed the database bytes")
	}
}

// TestProductionReviewRevisionLaneRequestShapes pins the two parsers to the wire
// shapes the reference registers: three request keys for the commit, one for the
// recovery, four handle keys for both.
func TestProductionReviewRevisionLaneRequestShapes(t *testing.T) {
	contextValue := projectsessionForReviewRevisionLane()
	digest := productionReviewRevisionLaneHash("an answer")
	handle := func() map[string]any {
		return map[string]any{
			"projectId":    "proj-1",
			"epoch":        "lease-1",
			"rootActionId": "root-1",
			"runId":        "run-1",
		}
	}
	valid := map[string]any{
		"contextId":              "00000000-0000-4000-8000-000000000001",
		"handle":                 handle(),
		"expectedCompositionHash": digest,
	}
	parsed, err := productionReviewRevisionCommitRevisionRequest(valid, contextValue)
	if err != nil {
		t.Fatalf("the production parser refused a well-formed commit: %v", err)
	}
	if parsed.ContextID != "00000000-0000-4000-8000-000000000001" ||
		parsed.Request.ProjectID != "proj-1" || parsed.Request.LiveEpoch != "lease-1" ||
		parsed.Request.Handle.ProjectID != "proj-1" || parsed.Request.Handle.Epoch != "lease-1" ||
		parsed.Request.Handle.RootActionID != "root-1" || parsed.Request.Handle.RunID != "run-1" ||
		parsed.Request.ExpectedCompositionHash != digest || parsed.Request.AssertPrepared != nil {
		t.Fatalf("the parser read the commit as %+v", parsed)
	}
	for _, arm := range []struct {
		label  string
		mutate func(map[string]any)
	}{
		{"an extra request key", func(request map[string]any) { request["extra"] = true }},
		{"a missing context id", func(request map[string]any) { delete(request, "contextId") }},
		{"a blank context id", func(request map[string]any) { request["contextId"] = "   " }},
		{"a missing composition hash", func(request map[string]any) {
			delete(request, "expectedCompositionHash")
		}},
		{"a blank composition hash", func(request map[string]any) { request["expectedCompositionHash"] = "" }},
		{"a handle that is not an object", func(request map[string]any) { request["handle"] = "root-1" }},
		{"an extra handle key", func(request map[string]any) {
			request["handle"].(map[string]any)["extra"] = true
		}},
		{"a missing handle key", func(request map[string]any) {
			delete(request["handle"].(map[string]any), "runId")
		}},
		{"a blank run id", func(request map[string]any) {
			request["handle"].(map[string]any)["runId"] = " "
		}},
	} {
		request := map[string]any{
			"contextId":               valid["contextId"],
			"handle":                  handle(),
			"expectedCompositionHash": digest,
		}
		arm.mutate(request)
		if _, err := productionReviewRevisionCommitRevisionRequest(request, contextValue); err == nil {
			t.Fatalf("the production parser accepted %s", arm.label)
		}
	}

	recovery, err := productionReviewRevisionRecoveryRequest(map[string]any{"handle": handle()}, contextValue)
	if err != nil {
		t.Fatalf("the production parser refused a well-formed recovery: %v", err)
	}
	if recovery.ProjectID != "proj-1" || recovery.Handle.RunID != "run-1" ||
		recovery.Handle.RootActionID != "root-1" || recovery.Handle.Epoch != "lease-1" ||
		recovery.Handle.ProjectID != "proj-1" {
		t.Fatalf("the parser read the recovery as %+v", recovery)
	}
	for _, arm := range []struct {
		label  string
		value  map[string]any
		refuse bool
	}{
		{"a missing handle", map[string]any{}, true},
		{"an extra request key", map[string]any{"handle": handle(), "runId": "run-1"}, true},
		{"a handle that is not an object", map[string]any{"handle": "run-1"}, true},
		{"a handle missing its epoch", map[string]any{"handle": map[string]any{
			"projectId": "proj-1", "rootActionId": "root-1", "runId": "run-1"}}, true},
		{"a handle with a blank project", map[string]any{"handle": map[string]any{
			"projectId": " ", "epoch": "lease-1", "rootActionId": "root-1", "runId": "run-1"}}, true},
	} {
		if _, err := productionReviewRevisionRecoveryRequest(arm.value, contextValue); (err != nil) != arm.refuse {
			t.Fatalf("the recovery parser answered %v for %s", err, arm.label)
		}
	}
}

// TestProductionReviewRevisionLaneRefusalCodes proves the two names' codes are
// reachable at the boundary: each new sentinel resolves to its own string through
// the one mapping table, a neighbour's code still resolves through the family
// table, and an unnamed failure never widens into either.
func TestProductionReviewRevisionLaneRefusalCodes(t *testing.T) {
	for _, arm := range []struct {
		refusal error
		code    string
	}{
		{store.ErrRevisionCommitCompositionRequired, "GENERATION_REVIEW_COMPOSITION_REQUIRED"},
		{store.ErrRevisionCommitEmpty, "GENERATION_REVIEW_REVISION_EMPTY"},
		{store.ErrRevisionCommitIncomplete, "GENERATION_REVIEW_REVISION_INCOMPLETE"},
		{store.ErrRevisionCommitNoop, "GENERATION_REVIEW_REVISION_NOOP"},
		{store.ErrReviewCommitInvalid, "GENERATION_REVIEW_COMMIT_INVALID"},
		{store.ErrReviewCommitConflict, "GENERATION_REVIEW_COMMIT_CONFLICT"},
		{store.ErrReviewCommitArtifact, "GENERATION_REVIEW_ARTIFACT_INVALID"},
		{store.ErrReviewCommitIdentity, "GENERATION_RUN_IDENTITY_MISMATCH"},
		{store.ErrReviewCommitCancelled, "GENERATION_ACTION_CANCELLED"},
		{store.ErrReviewCommitReceipt, "GENERATION_REVIEW_RECEIPT_INVALID"},
		{store.ErrReviewCommitContextRequired, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{store.ErrReviewCommitContextUnproven, "GENERATION_REVIEW_CONTEXT_INVALID"},
		{store.ErrReviewCommitContextChanged, "GENERATION_REVIEW_CONTEXT_CHANGED"},
	} {
		if got := productionReviewRevisionCommitCode(arm.refusal); got != arm.code {
			t.Fatalf("a named refusal resolves to %v instead of its own code", got)
		}
		if got := productionReviewRevisionCommitFailure(arm.refusal).Error(); got != arm.code {
			t.Fatalf("the browser would receive %v instead of %v", got, arm.code)
		}
	}
	// The store's composition reader raises its own code; it must survive as
	// itself rather than collapsing into the family's invalid answer.
	if got := productionReviewRevisionCommitFailure(errors.New("GENERATION_COMPOSITION_INTEGRITY_FAILED")).Error(); got !=
		"GENERATION_COMPOSITION_INTEGRITY_FAILED" {
		t.Fatalf("a symbolic store code answered %v", got)
	}
	// A failure this lane does not name is the family's invalid code, never a
	// SQLite message and never a draft body.
	if got := productionReviewRevisionCommitFailure(errors.New("sqlite3: disk I/O error")).Error(); got !=
		errProductionReviewRevisionInvalid.Error() {
		t.Fatalf("an unnamed failure answered %v", got)
	}
}

// projectsessionForReviewRevisionLane is the frozen session the two parsers are
// driven with: a project and a lease, no path.
func projectsessionForReviewRevisionLane() projectsession.Context {
	return projectsession.Context{ProjectID: "proj-1", LeaseID: "lease-1"}
}
