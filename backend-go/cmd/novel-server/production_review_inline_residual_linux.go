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
// # editor-inline: the two names that stay unbound
//
// `read-recovery` (production_editor_inline_linux.go:168) and `cancel` (:169) are
// served: both read the manifest of a run this process itself wrote, so neither
// needs anything outside this lane. The other two are not missing an adapter, they
// are missing a port:
//
//   - `execute` IS the model call. In the reference it resolves a lease, builds the
//     prompt for one frozen selection, dials the provider, and settles the artifact
//     (electron/services/editor-inline-generation.ts). In this module that whole
//     path is itself a browser-facing family: llm:begin-execution-lease,
//     llm:generate, llm:generate-stream, llm:cancel and llm:close-execution-lease
//     (production_llm_execution_linux.go:201-205, admitted through
//     internal/llmlease :163 Begin and :210 Resolve). The renderer performs its own
//     model call there and settles its own artifact; there is no server-side call
//     an editor-inline execute could hand an inline selection to, and its request
//     is an inline range of the author's paragraph rather than the chapter-draft
//     plan those entry points admit. Serving it would therefore need either a new
//     leased entry point - a change to the model authority - or a second provider
//     path inside this file, which would be an unauthorized dial.
//
//   - `begin` needs the builtin prompt corpus. Its one required template key is
//     `edit_selected_text`, and no Go file in this module names that key at all: the
//     only prompt loader is internal/promptskills/service.go:29, which reads the
//     author's installed prompts/*.json from the project directory and carries no
//     builtin templates. A begin that minted a handle without the template would
//     hand the renderer a context whose frozen prompt no execute could ever fill.
//     The second half is the admission list: begin's selection is not the ordinary
//     chapter-draft selection, so the reduced allowlist the ordinary lane admits
//     does not cover it, and widening that allowlist is a change to the model
//     authority rather than to this family.
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

// productionInlineResidualEditorServed is the half of the editor-inline family this
// lane binds, in the order web/operations.json lists it (read-recovery before
// execute, cancel last).
func productionInlineResidualEditorServed() []string {
	return []string{productionEditorInlineReadRecoveryName, productionEditorInlineCancelName}
}

// productionInlineResidualReviewServed is the review-revision family: all four names
// are bound now.
func productionInlineResidualReviewServed() []string {
	return []string{productionReviewRevisionPrepare, productionReviewRevisionCommitReview,
		productionReviewRevisionRevision, productionReviewRevisionRecovery}
}

// productionInlineResidualEditorSeams is this record's answer for the two
// editor-inline names the lane does not bind. Both are refused for a missing port,
// not for an unfinished adapter: the adapter already reads the run's own manifest
// (production_editor_inline_linux.go:526) and already projects the frozen input
// (:611), which is everything begin and execute would need if the prompt template
// and the model path existed.
func productionInlineResidualEditorSeams() []productionInlineResidualSeam {
	template := productionInlineResidualPort{
		Path: "../../internal/promptskills/service.go", Line: 29, Marker: "func (s *Service) LoadPrompts()",
		Because: "the only prompt loader in this module reads the author's installed prompts/*.json; begin's template key edit_selected_text is a builtin and no Go file names it",
	}
	generate := productionInlineResidualPort{
		Path: "production_llm_execution_linux.go", Line: 361, Marker: "func (x *productionLLMExecution) generate(",
		Because: "llm:generate is where a model call is admitted, and it is the renderer that calls it",
	}
	stream := productionInlineResidualPort{
		Path: "production_llm_execution_linux.go", Line: 382, Marker: "func (x *productionLLMExecution) generateStream(",
		Because: "the streaming twin of the same dispatch",
	}
	begin := productionInlineResidualPort{
		Path: "../../internal/llmlease/lease.go", Line: 163, Marker: "func (r *Registry) Begin(",
		Because: "no model call happens without a lease, and this is the only place one is minted",
	}
	resolve := productionInlineResidualPort{
		Path: "../../internal/llmlease/lease.go", Line: 210, Marker: "func (r *Registry) Resolve(",
		Because: "a resumed run is admitted by resolving the lease it was started under",
	}
	return []productionInlineResidualSeam{
		{
			Operation: productionEditorInlineBeginName,
			Family:    "editor-inline",
			Missing:   []productionInlineResidualPort{template, generate, begin},
			Answer: "serve it once the builtin prompt corpus carries edit_selected_text and the admitted selection covers an inline range; " +
				"until then a handle it minted could not be executed by any port in this lane",
		},
		{
			Operation: productionEditorInlineExecuteName,
			Family:    "editor-inline",
			Missing:   []productionInlineResidualPort{generate, stream, begin, resolve},
			Answer: "serve it only through the leased dispatch above, for an inline selection that dispatch admits; " +
				"a second provider path in this file would be an unauthorized dial",
		},
	}
}

// productionInlineResidualReviewSeams is empty: all four review-revision names are
// bound. It stays as a function so the record's test can assert that emptiness
// instead of asserting the absence of a symbol.
func productionInlineResidualReviewSeams() []productionInlineResidualSeam {
	return nil
}
