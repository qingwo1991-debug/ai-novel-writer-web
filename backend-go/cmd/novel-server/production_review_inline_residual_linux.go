//go:build linux

package main

// The residual record of two families: every name web/operations.json lists for
// `review-revision` (:312-315) and `editor-inline` (:188-191), which of them this
// lane binds, and - for each one it does not - the code that would have to exist
// first. Nothing here installs a handler. A name whose only possible answer is a
// refusal stays unbound, so it keeps failing closed at GO_OPERATION_NOT_MIGRATED
// (internal/transport), which is the answer the desktop controller's own inventory
// check already understands for a name the server does not serve.
//
// # editor-inline: all four names served
//
// `read-recovery` (production_editor_inline_linux.go) and `cancel` (:169) serve the
// read and stop halves; `begin` and `execute` were refused here for two passes and
// are now served by production_editor_inline_write_linux.go. The two premises the
// refusal rested on were both false: the builtin template `edit_selected_text` does
// ship (src/services/builtin-prompt-templates.ts:212, en-US overlay at
// src/services/prompt-language.ts:141) and now arrives as a committed asset
// (internal/prompttemplate), and a model call exists that this lane owns - the same
// lease, executor and OrdinaryService generation:execute and
// graph-generation:execute dispatch through. The admission did not need widening
// either: the ledger's closed family allowlist refuses only the keys it names, and
// editorInline* is not one of them (internal/store/generation_ledger.go:495-526).
//
// # review-revision: the two names that stay unbound
//
// `prepare` and `commit-review` are served (production_review_revision_linux.go:
// 129-130). The remaining two carry their own reasons in that file's header
// (:74-106); this record keeps the machine-checked half of them:
//
//   - `commit-revision` needs the visible-text gate
//     (../../../src/shared/visible-text-integrity.ts:26) and the no-op gate
//     (../../../src/shared/review-cycle.ts:95). Neither has a port in this module:
//     they are Unicode property expressions over visible text, and without them a
//     truncated or a fenced-off revision would be persisted as the author's own
//     chapter. The write half is no longer the gap - the pending-revision write
//     with its replace semantics is internal/store/reviewrevision.go:301 - so the
//     refusal now rests on the gate alone, which is why it is still a refusal.
//
//   - `read-recovery` needs `sourceStatus` and `latestArtifact`. The first is
//     rebuildBinding, which has no Go port in this module
//     (../../internal/store/draft_commit_tx.go:56 records the same gap for the
//     draft commit); the second is the run's settled receipt, which only the
//     synthetic lane reads today. A recovery object that answered neither would
//     still be worse than this 501, because the renderer reads `canResume` from it
//     to decide whether to spend another model call.
//
// # Why this file exists
//
// The refusal reasons above live in comments in three files. A comment cannot fail
// a build. The table below is the same record as data, and the test that reads it
// opens every cited file at the cited line, so a moved declaration fails a test
// instead of leaving a note that points at nothing.

// productionInlineResidualPort is one location that has to exist for a refused name
// to be answerable. Path is relative to this package's directory, Line is 1-based,
// and Marker must appear on that line.
type productionInlineResidualPort struct {
	Path    string
	Line    int
	Marker  string
	Because string
}

// productionInlineResidualSeam is one refused name: the port it waits for, and the
// one sentence a future reader needs to know whether the refusal can be lifted.
type productionInlineResidualSeam struct {
	Operation string
	Family    string
	Missing   []productionInlineResidualPort
	Answer    string
}

// The names this file records, spelled once so the test and the record cannot
// drift apart. The two bound names already have constants in their own files.
const (
	productionEditorInlineBeginName   = "editor-inline:begin"
	productionEditorInlineExecuteName = "editor-inline:execute"
	// review-revision:commit-revision and :read-recovery are served now
	// (production_review_revision_commit_linux.go): the visible-text and no-op gates
	// turned out to be reproducible from counters the store already had, and the
	// recovery read is one deferred transaction. The two names are kept here only
	// because the family's served list below names them.
	productionReviewRevisionRevision  = "review-revision:commit-revision"
	productionReviewRevisionRecovery  = "review-revision:read-recovery"
)

// productionInlineResidualEditorServed is the editor-inline family in the order
// web/operations.json lists it: begin, read-recovery, execute, cancel.
func productionInlineResidualEditorServed() []string {
	return []string{productionEditorInlineBeginName, productionEditorInlineReadRecoveryName,
		productionEditorInlineExecuteName, productionEditorInlineCancelName}
}

// productionInlineResidualReviewServed is the review-revision family: all four names
// are bound now.
func productionInlineResidualReviewServed() []string {
	return []string{productionReviewRevisionPrepare, productionReviewRevisionCommitReview,
		productionReviewRevisionRevision, productionReviewRevisionRecovery}
}

// productionInlineResidualEditorSeams is empty: all four editor-inline names are
// bound. It stays as a function so the record's test can assert that emptiness
// instead of asserting the absence of a symbol.
func productionInlineResidualEditorSeams() []productionInlineResidualSeam {
	return nil
}

// productionInlineResidualReviewSeams is empty: all four review-revision names are
// bound. It stays as a function so the record's test can assert that emptiness
// instead of asserting the absence of a symbol.
func productionInlineResidualReviewSeams() []productionInlineResidualSeam {
	return nil
}
