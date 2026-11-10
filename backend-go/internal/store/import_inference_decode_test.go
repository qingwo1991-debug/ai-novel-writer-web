package store

// Synthetic coverage for the import-inference decode port
// (import_inference_decode.go) and for the raised `generated` arm that now uses
// it (character_proposal_pending.go, generation-character-proposal-proof.ts:97-102).
//
// Two layers, both with synthetic data only:
//
//   - the pure decoder against hand-written inference JSON: the contract accepts
//     what it defines and refuses malformed enums, roles, field sets, card
//     counts, relationship endpoints and self-references;
//   - the store proof over a real baseline-v7 file, seeded with one global-facts
//     operation, one committed effect carrying a generationRunHandle, and the
//     settled import-inference artifacts, so the accept (generated provenance),
//     the honest reject (null provenance) and the artifact-integrity refusal are
//     all proved end to end.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

// iiTestCard is one synthetic characterCards entry in the contract's own literal
// key order. age is a number here so the decode's String(number) step is
// exercised, and appearance carries surrounding whitespace so the decode's trim
// is exercised too.
func iiTestCard(name, role string, relationships []any) plotObject {
	return plotObject{
		{"name", name}, {"role", role}, {"gender", "女"}, {"age", int64(20)},
		{"appearance", " 清瘦 "}, {"personality", "冷静"}, {"background", "旧城"},
		{"abilities", "剑术"}, {"motivation", "复仇"}, {"relationships", relationships},
		{"arc", "闭环"}, {"notes", "无"},
		{"currentState", iiTestState()},
	}
}

func iiTestState() plotObject {
	return plotObject{
		{"location", "客栈"}, {"powerLevel", "三流"}, {"physicalState", "轻伤"},
		{"mentalState", "清醒"}, {"keyItems", "短刀"}, {"recentEvents", "入城"},
		{"updatedAtChapter", int64(1)},
	}
}

// iiTestInference is one complete contract body: novelConfig and
// architectureFiles in their closed shapes plus the caller's cards.
func iiTestInference(cards []any) plotObject {
	return plotObject{
		{"novelConfig", plotObject{
			{"genre", "武侠"}, {"subGenre", "复仇"}, {"targetAudience", "男频"},
			{"plotStructure", "three_act"}, {"narrativePOV", "third_limited"},
			{"coreOutline", "大纲"}, {"worldSetting", "江湖"}, {"goldenFinger", "剑冢"},
			{"protagonistProfile", "孤女"}, {"globalGuidance", "克制"},
		}},
		{"architectureFiles", plotObject{
			{"premise", "前情"}, {"worldbuilding", "世设"}, {"synopsis", "梗概"},
		}},
		{"characterCards", cards},
	}
}

// iiTestCards is the accepted three-card set: one protagonist, one antagonist,
// one supporting card, and two relationships whose targets are in the name set.
func iiTestCards() []any {
	return []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiTestCard("乙", "antagonist", []any{plotObject{{"target", "丙"}, {"relation", "仇敌"}}}),
		iiTestCard("丙", "supporting", []any{}),
	}
}

// iiTestReplaceField is a small ordered-object edit used to build malformed
// drafts out of valid ones.
func iiTestReplaceField(object plotObject, field string, value any) plotObject {
	return pfReplace(object, field, value)
}

func iiTestDropField(object plotObject, field string) plotObject {
	trimmed := plotObject{}
	for _, entry := range object {
		if entry.key != field {
			trimmed = append(trimmed, entry)
		}
	}
	return trimmed
}

func iiTestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := plotJSON(value)
	if err != nil {
		t.Fatalf("encode synthetic inference: %v", err)
	}
	return encoded
}

// TestIIDecodeImportInferenceAccepts proves the accepted contract: three cards
// decode, age is stringified, text is trimmed, and the reference's key order
// survives.
func TestIIDecodeImportInferenceAccepts(t *testing.T) {
	cards, err := iiDecodeImportInferenceJSON(iiTestJSON(t, iiTestInference(iiTestCards())))
	if err != nil {
		t.Fatalf("decode accepted contract: %v", err)
	}
	if len(cards) != 3 {
		t.Fatalf("decoded %v cards, want 3", len(cards))
	}
	first, ok := cards[0].(plotObject)
	if !ok {
		t.Fatalf("card is %T, want the ordered object", cards[0])
	}
	wantKeys := []string{"name", "role", "gender", "age", "appearance", "personality",
		"background", "abilities", "motivation", "relationships", "arc", "notes", "currentState"}
	if len(first) != len(wantKeys) {
		t.Fatalf("card has %v keys, want %v", len(first), len(wantKeys))
	}
	for index, key := range wantKeys {
		if first[index].key != key {
			t.Fatalf("card key %v = %v, want %v", index, first[index].key, key)
		}
	}
	if got := pfText(first, "age"); got != "20" {
		t.Fatalf("age = %v, want the number stringified to 20", got)
	}
	if got := pfText(first, "appearance"); got != "清瘦" {
		t.Fatalf("appearance = %v, want the trimmed text", got)
	}
	if got := pfText(pfObject(pfValue(first, "currentState")), "location"); got != "客栈" {
		t.Fatalf("currentState.location = %v", got)
	}
}

// TestIIDecodeImportInferenceAcceptsWrapper proves the wrapper trim and the
// single-object extraction: invisible residue and surrounding prose are not part
// of the document.
func TestIIDecodeImportInferenceAcceptsWrapper(t *testing.T) {
	body := iiTestJSON(t, iiTestInference(iiTestCards()))
	wrapped := "\uFEFF\n 以下是推演结果：\n" + body + "\n\u200B "
	cards, err := iiDecodeImportInferenceJSON(wrapped)
	if err != nil {
		t.Fatalf("decode wrapped contract: %v", err)
	}
	if len(cards) != 3 {
		t.Fatalf("decoded %v cards, want 3", len(cards))
	}
}

// TestIIDecodeImportInferenceAcceptsSharedNameSelfReference proves the one
// self-reference the contract keeps: two occurrences share a label, so neither
// relation is a self-reference against a unique identity.
func TestIIDecodeImportInferenceAcceptsSharedNameSelfReference(t *testing.T) {
	cards := []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "甲"}, {"relation", "同名"}}}),
		iiTestCard("甲", "supporting", []any{}),
		iiTestCard("乙", "supporting", []any{}),
	}
	decoded, err := iiDecodeImportInferenceJSON(iiTestJSON(t, iiTestInference(cards)))
	if err != nil {
		t.Fatalf("decode shared-name contract: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("decoded %v cards, want 3", len(decoded))
	}
}

// TestIIDecodeImportInferenceRefuses proves the malformed shapes are refusals
// rather than partial decodes. Every case is synthetic JSON text.
func TestIIDecodeImportInferenceRefuses(t *testing.T) {
	roleOutside := iiTestCards()
	roleOutside[0] = iiTestReplaceField(roleOutside[0].(plotObject), "role", "hero")
	missingField := iiTestCards()
	missingField[0] = iiTestDropField(missingField[0].(plotObject), "notes")
	relationshipsNotArray := iiTestCards()
	relationshipsNotArray[0] = iiTestReplaceField(relationshipsNotArray[0].(plotObject), "relationships", "旧识")
	selfReference := iiTestCards()
	selfReference[0] = iiTestReplaceField(selfReference[0].(plotObject), "relationships",
		[]any{plotObject{{"target", "甲"}, {"relation", "自省"}}})
	endpointOutside := iiTestCards()
	endpointOutside[0] = iiTestReplaceField(endpointOutside[0].(plotObject), "relationships",
		[]any{plotObject{{"target", "丁"}, {"relation", "陌路"}}})
	emptyText := iiTestCards()
	emptyText[1] = iiTestReplaceField(emptyText[1].(plotObject), "background", "   ")
	negativeChapter := iiTestCards()
	negativeChapter[2] = iiTestReplaceField(negativeChapter[2].(plotObject), "currentState",
		iiTestReplaceField(iiTestState(), "updatedAtChapter", int64(-1)))
	badStructure := iiTestInference(iiTestCards())
	badStructure = iiTestReplaceField(badStructure, "novelConfig",
		iiTestReplaceField(pfObject(pfValue(badStructure, "novelConfig")), "plotStructure", "四幕"))
	noCards := plotObject{
		{"novelConfig", pfValue(iiTestInference(nil), "novelConfig")},
		{"architectureFiles", pfValue(iiTestInference(nil), "architectureFiles")},
	}
	cases := []struct {
		label string
		body  string
	}{
		{"not JSON at all", "不是推演结果"},
		{"two complete objects", "{}{}"},
		{"no object", `"characterCards": []`},
		{"two cards", iiTestJSON(t, iiTestInference([]any{
			iiTestCard("甲", "protagonist", []any{}),
			iiTestCard("乙", "supporting", []any{}),
		}))},
		{"nine cards", iiTestJSON(t, iiTestInference([]any{
			iiTestCard("甲", "protagonist", []any{}), iiTestCard("乙", "supporting", []any{}),
			iiTestCard("丙", "supporting", []any{}), iiTestCard("丁", "supporting", []any{}),
			iiTestCard("戊", "supporting", []any{}), iiTestCard("己", "supporting", []any{}),
			iiTestCard("庚", "supporting", []any{}), iiTestCard("辛", "supporting", []any{}),
			iiTestCard("壬", "supporting", []any{}),
		}))},
		{"no protagonist", iiTestJSON(t, iiTestInference([]any{
			iiTestCard("甲", "supporting", []any{}), iiTestCard("乙", "supporting", []any{}),
			iiTestCard("丙", "supporting", []any{}),
		}))},
		{"role outside the vocabulary", iiTestJSON(t, iiTestInference(roleOutside))},
		{"missing field", iiTestJSON(t, iiTestInference(missingField))},
		{"relationships not an array", iiTestJSON(t, iiTestInference(relationshipsNotArray))},
		{"unique name relates to itself", iiTestJSON(t, iiTestInference(selfReference))},
		{"endpoint outside the name set", iiTestJSON(t, iiTestInference(endpointOutside))},
		{"plotStructure outside the enum", iiTestJSON(t, badStructure)},
		{"empty required text", iiTestJSON(t, iiTestInference(emptyText))},
		{"negative updatedAtChapter", iiTestJSON(t, iiTestInference(negativeChapter))},
		{"missing characterCards", iiTestJSON(t, noCards)},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			if cards, err := iiDecodeImportInferenceJSON(testCase.body); err == nil {
				t.Fatalf("decode accepted %v and answered %#v", testCase.label, cards)
			}
		})
	}
	// The accepted set is the control: the same builder passes as-is.
	if _, err := iiDecodeImportInferenceJSON(iiTestJSON(t, iiTestInference(iiTestCards()))); err != nil {
		t.Fatalf("control decode: %v", err)
	}
}

// TestIIEndpointDelta proves the bounded correction: a delta that closes the
// single unresolved target is accepted and merged, and a delta with an extra
// key, a new name, or the wrong count is refused.
func TestIIEndpointDelta(t *testing.T) {
	original := []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiTestCard("丙", "supporting", []any{}),
		iiTestCard("丁", "supporting", []any{}),
	}
	originalJSON := iiTestJSON(t, iiTestInference(original))
	deltaCard := iiTestCard("乙", "supporting", []any{})
	delta := iiTestJSON(t, plotObject{{"characterCards", []any{deltaCard}}})
	decoded, err := iiDecodeImportInferenceWithEndpointDelta(originalJSON, delta)
	if err != nil {
		t.Fatalf("decode corrected contract: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("decoded %v cards, want the original three plus the correction", len(decoded))
	}
	if got := pfText(decoded[3].(plotObject), "name"); got != "乙" {
		t.Fatalf("correction card = %v, want 乙", got)
	}
	for _, testCase := range []struct {
		label string
		body  string
	}{
		{"an extra root key", iiTestJSON(t, plotObject{
			{"characterCards", []any{deltaCard}}, {"novelConfig", plotObject{}},
		})},
		{"an extra card key", iiTestJSON(t, plotObject{{"characterCards", []any{
			iiTestReplaceField(deltaCard, "extra", "值"),
		}}})},
		{"a name outside the unresolved set", iiTestJSON(t, plotObject{{"characterCards", []any{
			iiTestCard("戊", "supporting", []any{}),
		}}})},
		{"the wrong card count", iiTestJSON(t, plotObject{{"characterCards", []any{
			deltaCard, iiTestCard("乙", "supporting", []any{}),
		}}})},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			if cards, err := iiDecodeImportInferenceWithEndpointDelta(originalJSON, testCase.body); err == nil {
				t.Fatalf("delta accepted %v and answered %#v", testCase.label, cards)
			}
		})
	}
}

// TestIIPreservesStructuredJsonEvidence proves the repair boundary: appended
// closers keep the evidence, a changed scalar does not.
func TestIIPreservesStructuredJsonEvidence(t *testing.T) {
	candidate := `{"characterCards":[{"name":"甲"}]`
	if !iiPreservesStructuredJSONEvidence(candidate, candidate+"}") {
		t.Fatalf("appended closer was refused")
	}
	if iiPreservesStructuredJSONEvidence(candidate, `{"characterCards":[{"name":"乙"}]}`) {
		t.Fatalf("a changed scalar was accepted")
	}
	if iiPreservesStructuredJSONEvidence(candidate, `{"characterCards":[{"name":"甲"},{"name":"甲"}]}`) {
		t.Fatalf("a duplicated container was accepted")
	}
	if iiPreservesStructuredJSONEvidence(candidate, `{"characterCards":[{"name":"甲"}]`) {
		t.Fatalf("an unparsable repair was accepted")
	}
}

// iiDecodedEntry is the receipt's frozen shape for one card after the decode: age
// stringified, text trimmed, relationship keys in the reference's order.
func iiDecodedEntry(name, role string, relationships []any) plotObject {
	return plotObject{
		{"name", name}, {"role", role}, {"gender", "女"}, {"age", "20"},
		{"appearance", "清瘦"}, {"personality", "冷静"}, {"background", "旧城"},
		{"abilities", "剑术"}, {"motivation", "复仇"}, {"relationships", relationships},
		{"arc", "闭环"}, {"notes", "无"},
		{"currentState", iiTestState()},
	}
}

// iiGeneratedEntries is the entry set the accepted artifact decodes to.
func iiGeneratedEntries() []any {
	return []any{
		iiDecodedEntry("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiDecodedEntry("乙", "antagonist", []any{plotObject{{"target", "丙"}, {"relation", "仇敌"}}}),
		iiDecodedEntry("丙", "supporting", []any{}),
	}
}

// iiGeneratedFixture is one baseline-v7 file with the whole generated arm seeded:
// the global-facts operation and its receipt, the committed effect that names the
// run handle, the run whose binding freezes the import slot and the model, and
// the settled import-inference artifacts.
type iiGeneratedFixture struct {
	db            *sql.DB
	projectID     string
	operationID   string
	epoch         string
	rootActionID  string
	runID         string
	attemptID     string
	artifactID    string
	modelRevision string
	entries       []any
}

const (
	iiGeneratedProjectID = "proj-import-generated"
	iiGeneratedOperation = "novel-import-global-ci-generated"
	iiGeneratedEpoch     = "epoch-import-generated"
	iiGeneratedRootID    = "root-import-generated"
	iiGeneratedRunID     = "run-import-generated"
	iiGeneratedAttemptID = "attempt-import-generated"
	iiGeneratedArtifact  = "artifact-import-generated"
	iiGeneratedModelHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// iiGeneratedFixtureOpen provisions the pinned v7 schema in a temp directory and
// opens it, so this lane's proof runs on the real DDL rather than on a stub.
func iiGeneratedFixtureOpen(t *testing.T) *iiGeneratedFixture {
	t.Helper()
	return iiGeneratedFixtureOpenWith(t, iiGeneratedEntries())
}

func iiGeneratedFixtureOpenWith(t *testing.T, entries []any) *iiGeneratedFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import inference #? 中文.db")
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000&_foreign_keys=on"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.WriteBaselineV7(path, seed); err != nil {
		_ = seed.Close()
		t.Fatalf("provision baseline v7: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	fixture := &iiGeneratedFixture{
		db:            db,
		projectID:     iiGeneratedProjectID,
		operationID:   iiGeneratedOperation,
		epoch:         iiGeneratedEpoch,
		rootActionID:  iiGeneratedRootID,
		runID:         iiGeneratedRunID,
		attemptID:     iiGeneratedAttemptID,
		artifactID:    iiGeneratedArtifact,
		modelRevision: iiGeneratedModelHash,
		entries:       entries,
	}
	fixture.seed(t)
	return fixture
}

func iiGeneratedJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := plotJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// seed writes the operation, the effect, the run, its one settled inference
// attempt and the run's binding.
func (f *iiGeneratedFixture) seed(t *testing.T) {
	t.Helper()
	db := f.db
	fingerprint := plotObject{{"modelId", "ci-model"}, {"policyRevision", int64(3)}}
	handle := plotObject{
		{"projectId", f.projectID}, {"epoch", f.epoch},
		{"rootActionId", f.rootActionID}, {"runId", f.runID},
	}
	// The operation's payload_hash is the hash of the receipt's own proposalSource
	// (:72-73), while the effect row is hashed over its own payload_json (:88).
	// They are two different columns, so the fixture hashes each over its own bytes.
	proposalSource := plotObject{{"characterEntries", f.entries}}
	sourceHash := summaryHash(iiGeneratedJSON(t, proposalSource))
	receipt := plotObject{
		{"operationId", f.operationID},
		{"payloadHash", sourceHash},
		{"proposalSource", proposalSource},
	}
	mustExec(t, db, `INSERT INTO import_global_fact_operations(operation_id,payload_hash,receipt_json) VALUES(?,?,?)`,
		f.operationID, sourceHash, iiGeneratedJSON(t, receipt))
	effectPayload := plotObject{
		{"operationId", f.operationID},
		{"characterEntries", f.entries},
		{"generationRunHandle", handle},
	}
	effectText := iiGeneratedJSON(t, effectPayload)
	mustExec(t, db, `INSERT INTO import_runs(id,purpose,root_run_id,effect_namespace,source_fingerprint,
 manifest_fingerprint,locale,total_chapters,manifest_chapter_count) VALUES(?,?,?,?,?,?,?,?,?)`,
		"ci-run-generated", "reference", "ci-root-generated", "ci-ns-generated", "src-fp", "man-fp", "zh-CN", 1, 1)
	mustExec(t, db, `INSERT INTO import_run_receipts(run_id,schema_version,effect_namespace,effect_key,
 stage,batch_id,kind,payload_json,payload_hash,state) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"ci-run-generated", 1, "ci-ns-generated", "ci-key-generated", "global", "ci-batch-generated",
		"project-global-facts", effectText, summaryHash(effectText), "committed")
	mustExec(t, db, `INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json) VALUES(?,?,?,?)`,
		f.rootActionID, "idem-import-generated",
		iiGeneratedJSON(t, plotObject{{"rootActionId", f.rootActionID}, {"status", "active"}}),
		iiGeneratedJSON(t, plotObject{{"policy", plotObject{{"maxAttempts", int64(3)}}}}))
	binding := plotObject{
		{"projectId", f.projectID},
		{"epoch", f.epoch},
		{"fingerprint", fingerprint},
		{"sourceManifest", plotObject{
			{"operation", "import-global-facts"},
			{"importSlot", plotObject{{"runId", "ci-run-generated"}, {"stage", "global"}, {"batchId", "ci-batch-generated"}}},
			{"modelReceipt", plotObject{{"modelRevision", f.modelRevision}}},
		}},
	}
	mustExec(t, db, `INSERT INTO generation_runs(run_id,root_action_id,binding_json,status,created_at_ms,open_key)
 VALUES(?,?,?,?,?,?)`, f.runID, f.rootActionID, iiGeneratedJSON(t, binding), "running", 1, "open-import-generated")
	attempt := plotObject{{"attemptId", f.attemptID}, {"runId", f.runID}, {"status", "settled"}}
	usage := plotObject{
		{"purpose", iiInferencePurpose},
		{"result", plotObject{{"finishReason", "stop"}}},
		{"artifactIdentity", plotObject{{"artifactId", f.artifactID}, {"epoch", f.epoch}, {"fingerprint", fingerprint}}},
	}
	mustExec(t, db, `INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
 attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`,
		f.attemptID, "reservation-import-generated", f.runID, f.rootActionID,
		iiGeneratedJSON(t, attempt), iiGeneratedJSON(t, usage), "nonce-import-generated")
}

// artifactJSON is the ledger artifact body the receipt re-proves: the text and
// the hash claimed beside it are parameters, so a tampered artifact can be seeded
// without touching the schema's own JSON functions.
func (f *iiGeneratedFixture) artifactJSON(t *testing.T, artifactID, attemptID, text, textHash string) string {
	t.Helper()
	return iiGeneratedJSON(t, plotObject{
		{"artifactId", artifactID},
		{"attemptId", attemptID},
		{"rootActionId", f.rootActionID},
		{"projectId", f.projectID},
		{"epoch", f.epoch},
		{"fingerprint", plotObject{{"modelId", "ci-model"}, {"policyRevision", int64(3)}}},
		{"revision", int64(0)},
		{"text", text},
		{"textHash", textHash},
	})
}

// writeArtifact stores one artifact body for the inference attempt, with the hash
// the ledger receipt re-proves.
func (f *iiGeneratedFixture) writeArtifact(t *testing.T, text string) {
	t.Helper()
	mustExec(t, f.db, `INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status)
 VALUES(?,?,?,?,?,?)`, f.artifactID, f.attemptID, f.runID,
		f.artifactJSON(t, f.artifactID, f.attemptID, text, summaryHash(text)), 0, "settled")
}

// prove runs the import arm over the fixture.
func (f *iiGeneratedFixture) prove(t *testing.T) (map[string]any, error) {
	t.Helper()
	return NewPendingFinalized(f.db).Prove(context.Background(), f.projectID,
		map[string]any{"kind": "import", "operationId": f.operationID})
}

// TestPendingFinalizedImportGeneratedProvenance is the raised arm (:97-102): an
// artifact that decodes to exactly the receipt's entries proves a `generated`
// provenance naming the run's model and the source hash.
func TestPendingFinalizedImportGeneratedProvenance(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	fixture.writeArtifact(t, iiTestJSON(t, iiTestInference(iiTestCards())))
	proof, err := fixture.prove(t)
	if err != nil {
		t.Fatalf("prove generated source: %v", err)
	}
	provenance, ok := proof["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance = %#v, want the generated object", proof["provenance"])
	}
	if provenance["kind"] != "generated" {
		t.Fatalf("provenance.kind = %v, want generated", provenance["kind"])
	}
	if provenance["modelRevision"] != fixture.modelRevision {
		t.Fatalf("modelRevision = %v, want %v", provenance["modelRevision"], fixture.modelRevision)
	}
	source, ok := provenance["source"].(map[string]any)
	if !ok {
		t.Fatalf("provenance.source = %#v, want the source object", provenance["source"])
	}
	if source["projectId"] != fixture.projectID || source["epoch"] != fixture.epoch ||
		source["sourceId"] != fixture.operationID {
		t.Fatalf("provenance.source identity = %#v", source)
	}
	if source["revision"] != int64(0) {
		t.Fatalf("provenance.source.revision = %#v, want 0", source["revision"])
	}
	if source["contentHash"] != proof["sourceHash"] {
		t.Fatalf("contentHash = %v, want the source hash %v", source["contentHash"], proof["sourceHash"])
	}
}

// TestPendingFinalizedImportGeneratedMismatchIsNull is :98: a run whose
// artifact decodes to different entries proves the null provenance rather than a
// guessed generation.
func TestPendingFinalizedImportGeneratedMismatchIsNull(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	cards := []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiTestCard("乙", "antagonist", []any{plotObject{{"target", "丁"}, {"relation", "仇敌"}}}),
		iiTestCard("丁", "supporting", []any{}),
	}
	fixture.writeArtifact(t, iiTestJSON(t, iiTestInference(cards)))
	proof, err := fixture.prove(t)
	if err != nil {
		t.Fatalf("prove mismatched source: %v", err)
	}
	if provenance, present := proof["provenance"]; !present || provenance != nil {
		t.Fatalf("provenance = %#v, want null for a different decode", proof["provenance"])
	}
}

// TestPendingFinalizedImportGeneratedMalformedIsNull is :40: a contract the
// decoder refuses is caught into the null arm, not answered as a provenance and
// not refused by name.
func TestPendingFinalizedImportGeneratedMalformedIsNull(t *testing.T) {
	badRole := iiTestCards()
	badRole[0] = iiTestReplaceField(badRole[0].(plotObject), "role", "hero")
	endpointOutside := []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "丁"}, {"relation", "陌路"}}}),
		iiTestCard("乙", "supporting", []any{}),
		iiTestCard("丙", "supporting", []any{}),
	}
	for _, testCase := range []struct {
		label string
		text  string
	}{
		{"not JSON", "不是推演结果"},
		{"a card role outside the vocabulary", iiTestJSON(t, iiTestInference(badRole))},
		{"an endpoint outside the name set", iiTestJSON(t, iiTestInference(endpointOutside))},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			fixture := iiGeneratedFixtureOpen(t)
			fixture.writeArtifact(t, testCase.text)
			proof, err := fixture.prove(t)
			if err != nil {
				t.Fatalf("prove malformed artifact: %v", err)
			}
			if provenance, present := proof["provenance"]; !present || provenance != nil {
				t.Fatalf("provenance = %#v, want null for %v", proof["provenance"], testCase.label)
			}
		})
	}
}

// TestPendingFinalizedImportGeneratedTamperedArtifactRefuses is :26-28: an
// artifact whose text no longer hashes to its recorded hash is not a source at
// all, so the proof raises the reference's own SOURCE_INVALID rather than
// falling back to null.
func TestPendingFinalizedImportGeneratedTamperedArtifactRefuses(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	text := iiTestJSON(t, iiTestInference(iiTestCards()))
	mustExec(t, fixture.db, `INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status)
 VALUES(?,?,?,?,?,?)`, fixture.artifactID, fixture.attemptID, fixture.runID,
		fixture.artifactJSON(t, fixture.artifactID, fixture.attemptID, "被篡改的正文", summaryHash(text)), 0, "settled")
	_, err := fixture.prove(t)
	if !errors.Is(err, ErrCharacterProposalPendingSourceInvalid) {
		t.Fatalf("prove tampered artifact = %v, want %v", err, ErrCharacterProposalPendingSourceInvalid)
	}
}

// TestPendingFinalizedImportGeneratedRepairAndCorrection proves the two bounded
// artifacts compose the way the reference composes them: a syntax repair that
// only appends the missing closer replaces the candidate, and an endpoint
// correction adds the missing target, so the merged decode equals the receipt's
// entries and proves `generated` rather than null.
func TestPendingFinalizedImportGeneratedRepairAndCorrection(t *testing.T) {
	entries := []any{
		iiDecodedEntry("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiDecodedEntry("丙", "supporting", []any{}),
		iiDecodedEntry("丁", "supporting", []any{}),
		iiDecodedEntry("乙", "supporting", []any{}),
	}
	fixture := iiGeneratedFixtureOpenWith(t, entries)
	cards := []any{
		iiTestCard("甲", "protagonist", []any{plotObject{{"target", "乙"}, {"relation", "同门"}}}),
		iiTestCard("丙", "supporting", []any{}),
		iiTestCard("丁", "supporting", []any{}),
	}
	repaired := iiGeneratedJSON(t, iiTestInference(cards))
	// The candidate the repair is asked to fix is the same document with its
	// final closer missing, so the repair only appends it.
	fixture.writeArtifact(t, strings.TrimSuffix(repaired, "}"))
	fixture.writeExtraAttempt(t, iiSyntaxPurpose, "attempt-generated-syntax", "artifact-generated-syntax", repaired)
	fixture.writeExtraAttempt(t, iiEndpointPurpose, "attempt-generated-endpoint", "artifact-generated-endpoint",
		iiGeneratedJSON(t, plotObject{{"characterCards", []any{iiTestCard("乙", "supporting", []any{})}}}))
	proof, err := fixture.prove(t)
	if err != nil {
		t.Fatalf("prove repaired source: %v", err)
	}
	provenance, ok := proof["provenance"].(map[string]any)
	if !ok || provenance["kind"] != "generated" {
		t.Fatalf("provenance = %#v, want the generated object for the repaired and corrected decode", proof["provenance"])
	}
}

// writeExtraAttempt stores one further attempt and artifact for the run, which is
// how the repair and correction purposes reach
// importCharacterEntriesFromArtifacts.
func (f *iiGeneratedFixture) writeExtraAttempt(t *testing.T, purpose, attemptID, artifactID, text string) {
	t.Helper()
	fingerprint := plotObject{{"modelId", "ci-model"}, {"policyRevision", int64(3)}}
	attempt := plotObject{{"attemptId", attemptID}, {"runId", f.runID}, {"status", "settled"}}
	usage := plotObject{
		{"purpose", purpose},
		{"result", plotObject{{"finishReason", "stop"}}},
		{"artifactIdentity", plotObject{{"artifactId", artifactID}, {"epoch", f.epoch}, {"fingerprint", fingerprint}}},
	}
	mustExec(t, f.db, `INSERT INTO generation_attempts(attempt_id,reservation_id,run_id,root_action_id,
 attempt_json,usage_receipt_json,invocation_nonce) VALUES(?,?,?,?,?,?,?)`,
		attemptID, "reservation-"+attemptID, f.runID, f.rootActionID,
		iiGeneratedJSON(t, attempt), iiGeneratedJSON(t, usage), "nonce-"+attemptID)
	mustExec(t, f.db, `INSERT INTO generation_artifacts(artifact_id,attempt_id,run_id,artifact_json,revision,status)
 VALUES(?,?,?,?,?,?)`, artifactID, attemptID, f.runID,
		f.artifactJSON(t, artifactID, attemptID, text, summaryHash(text)), 0, "settled")
}

// TestPendingFinalizedImportGeneratedRepairChangedScalarIsNull is :34: a
// repair that changed scalar evidence is refused by the evidence comparison, so
// the arm proves null instead of decoding the repaired text.
func TestPendingFinalizedImportGeneratedRepairChangedScalarIsNull(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	original := iiGeneratedJSON(t, iiTestInference(iiTestCards()))
	if !strings.Contains(original, "甲") {
		t.Fatalf("synthetic candidate does not carry the name the repair changes")
	}
	fixture.writeArtifact(t, original)
	fixture.writeExtraAttempt(t, iiSyntaxPurpose, "attempt-generated-syntax", "artifact-generated-syntax",
		strings.Replace(original, "甲", "戊", 1))
	proof, err := fixture.prove(t)
	if err != nil {
		t.Fatalf("prove repaired source: %v", err)
	}
	if provenance, present := proof["provenance"]; !present || provenance != nil {
		t.Fatalf("provenance = %#v, want null for a repair that changed a scalar", proof["provenance"])
	}
}

// TestPendingFinalizedImportGeneratedDoubledInferenceIsNull is :23: two
// inference attempts are not the one settled inference the arm decodes.
func TestPendingFinalizedImportGeneratedDoubledInferenceIsNull(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	fixture.writeArtifact(t, iiTestJSON(t, iiTestInference(iiTestCards())))
	fixture.writeExtraAttempt(t, iiInferencePurpose, "attempt-generated-second", "artifact-generated-second",
		iiTestJSON(t, iiTestInference(iiTestCards())))
	proof, err := fixture.prove(t)
	if err != nil {
		t.Fatalf("prove doubled inference: %v", err)
	}
	if provenance, present := proof["provenance"]; !present || provenance != nil {
		t.Fatalf("provenance = %#v, want null for a doubled inference", proof["provenance"])
	}
}

// TestPendingFinalizedImportGeneratedMissingRun proves the ledger's own
// missing-run refusal is preserved now that the raised arm reads the run.
func TestPendingFinalizedImportGeneratedMissingRun(t *testing.T) {
	fixture := iiGeneratedFixtureOpen(t)
	mustExec(t, fixture.db, `DELETE FROM generation_artifacts WHERE run_id=?`, fixture.runID)
	mustExec(t, fixture.db, `DELETE FROM generation_attempts WHERE run_id=?`, fixture.runID)
	mustExec(t, fixture.db, `DELETE FROM generation_runs WHERE run_id=?`, fixture.runID)
	_, err := fixture.prove(t)
	if err == nil || err.Error() != "GENERATION_RUN_MISSING" {
		t.Fatalf("prove missing run = %v, want GENERATION_RUN_MISSING", err)
	}
}
