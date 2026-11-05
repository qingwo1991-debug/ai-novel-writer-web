//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's adapter for the two smallest remaining
// families in web/operations.json: `chapter` (4 names) and `prompt` (3 names).
// It binds exactly one of those seven names and refuses the other six by not
// installing a handler for them, so they keep failing closed at
// 501 GO_OPERATION_NOT_MIGRATED instead of being published as handlers that
// could only refuse or, worse, that could write.
//
// # chapter: nothing here is bound, and nothing here may be
//
// The family is the finalized-chapter deletion lane, not a read lane:
//
//   - `chapter:delete-finalized` deletes the finalized fact and every derived
//     projection. Its first durable step is
//     `INSERT INTO chapter_deletion_operations ...`
//     (electron/repositories/chapter-deletion-repository.ts:280-301) followed by
//     `deleteChapterFacts` (:304). A Go binding would have to create that row
//     before it could delete anything, which is exactly the "backfill before a
//     select" this adapter refuses.
//   - `chapter:retry-deletion` re-runs the cleanup: it increments and re-pends
//     the row with `UPDATE chapter_deletion_operations SET attempt_count =
//     attempt_count + 1, status = 'pending'` (:402-406) and then deletes
//     manuscript/knowledge projections and marks each one completed
//     (`markProjection`, :420-424). It mutates a ledger and a filesystem.
//   - `chapter:confirm-legacy-knowledge-absent` consumes the legacy
//     authorization with `UPDATE chapter_deletion_operations SET
//     legacy_knowledge_authorization = 'consumed', knowledge_status =
//     'not_required', status = 'pending'` (:345-359) and then deletes the
//     finalized facts on the strength of that write. Binding it would let a
//     browser authorize an irreversible deletion this lane cannot perform.
//   - `chapter:list-incomplete-deletions` is the family's only SELECT
//     (`SELECT * FROM chapter_deletion_operations WHERE status != 'completed'
//     ...`, :380-386), but it is not a read this lane can answer honestly. It is
//     implemented only in the Electron main process
//     (electron/services/chapter-deletion-service.ts:132-135), it projects the
//     19-field ChapterDeletionOperation wire object
//     (src/shared/chapter-deletion.ts:10-30), and it filters every row through
//     the portable-runtime freeze document (`readPortableRuntimeFreeze`). The Go
//     backend has no part of that: `grep -rn chapter_deletion backend-go` and
//     `grep -rn '"chapter:' backend-go` both return nothing, so there is no
//     store repository, no row type, no freeze guard and no transport binder to
//     borrow. Answering it would mean writing raw SQL in package main over a
//     table nothing in this lane ever populates, and returning an empty list for
//     a project whose deletions the author's other tool already recorded -- a
//     fabricated 200. So all four names stay unbound here, and this file adds no
//     mutation to the lane.
//
// # prompt: one read bound, two writes refused
//
//   - `prompt:load-global` is bound. It takes ZERO arguments
//     (internal/transport/promptskills_read.go:55; the renderer calls it as
//     `ipc.invoke('prompt:load-global')`, src/services/prompt-catalog.ts:360), so
//     it carries no project path and no frozen lease. The handler is this file's
//     own operation rather than transport's BindPromptSkillsReads, because that
//     binder publishes `prompt:load-global` and `skills:list-user` atomically
//     (:51-57): taking it would bind a name from the skills family this task does
//     not own, and would make any later `skills` binder fail startup with
//     OPERATION_ALREADY_BOUND.
//   - `prompt:save-global` is refused: it writes the template through
//     `promptskills.SavePrompt` (internal/promptskills/service.go:34-37), which
//     `atomicWrite`s a new `prompts/<key>.<lang>.json` and removes the legacy
//     file. Arity 1 (:219).
//   - `prompt:delete-global` is refused: it removes prompt files through
//     `promptskills.DeletePrompt` (service.go:38-40) and `removeFile`
//     (internal/promptskills/files_linux.go:59). Arity 2 (:220).
//
// Neither write is installed, so both keep failing closed at
// 501 GO_OPERATION_NOT_MIGRATED and this lane never gains the capability to
// change the author's global prompt directory.

// productionPromptLoad is prompt:load-global over the private global prompt
// root the lane already pinned. It never opens a request-supplied path and never
// parses a lease: global reads carry none.
type productionPromptLoad struct {
	// global is the pinned global admission. The prompt root is taken from it,
	// through its own re-verifying DataRoot, and never from a caller field.
	global *globaladmission.PinnedReady
	// startup is the committed-generation capture the family's own binder gates
	// on, so an unidentified caller is refused with the same code it returns.
	startup *globalconfig.Startup
	mu      sync.Mutex
	// root is the verified committed global data root, resolved once. It has the
	// same lifetime as the global config service this lane already pins.
	root string
}

// newProductionPromptLoad returns the prompt:load-global handler for the lane
// built by productionHandlerWithContract. It resolves nothing at bind time: a
// root that cannot be verified is refused per request, so startup never depends
// on an optional global object being readable.
func newProductionPromptLoad(global *globaladmission.PinnedReady, startup *globalconfig.Startup) transport.Operation {
	return (&productionPromptLoad{global: global, startup: startup}).handle
}

// handle serves one prompt:load-global request.
func (p *productionPromptLoad) handle(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if p == nil {
		return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	// Arity first, exactly like the family binder: a stale lease or a project
	// path in this position is a malformed request, never authority.
	if len(args) != 0 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	// The family's caller gate (promptskills_read.go:66-70): an unidentified
	// caller never reaches the private root, and the code is the one the shipped
	// binder already returns.
	if key := productionGlobalCaller(caller); key == "" ||
		p.startup == nil || p.startup.State(key, true).State != "ready" {
		return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
	}
	if ctx == nil {
		return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	root, err := p.pinnedRoot()
	if err != nil {
		return nil, productionPromptError(err)
	}
	// A fresh pin per request: promptskills.New re-opens every ancestor without
	// following a symlink and re-checks the owner and the mode, so a directory
	// swapped underneath this lane is refused, and the descriptor is released
	// before the response is written.
	service, err := promptskills.New(root)
	if err != nil {
		return nil, productionPromptError(err)
	}
	defer func() { _ = service.Close() }()
	templates, err := service.LoadPrompts()
	if err != nil {
		return nil, productionPromptError(err)
	}
	return templates, nil
}

// pinnedRoot resolves the committed global data root from the pinned admission
// itself - never from a request, an environment variable or a re-derived path.
// PinnedReady.DataRoot re-verifies the receipt, the generation and the pinned
// directory chain before it answers, so the value it returns names the same root
// globalconfig.New already pinned for this process (production_open_linux.go:183)
// and that generation's `prompts/` objects are the ones this lane reads.
//
// A success is cached; a failure is not, so a generation that becomes readable
// later is served without a restart.
func (p *productionPromptLoad) pinnedRoot() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root != "" {
		return p.root, nil
	}
	if p.global == nil {
		return "", errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	// A full identity check reads the receipt and the generation documents, so it
	// is bounded the same way the author-database borrow bounds its refresh.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root, err := p.global.DataRoot(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	if root == "" {
		return "", errors.New("PROMPT_SKILLS_ROOT_UNAVAILABLE")
	}
	p.root = root
	return root, nil
}

// productionGlobalCaller is the identity a project-less global read is keyed by,
// copied from transport's own startup gate (globalconfig.go:95-99): a principal,
// a cookie session and a browser tab must all be named. It is only a map key for
// startup readiness; it grants nothing and names no path.
func productionGlobalCaller(caller transport.Caller) string {
	if caller.Principal == "" || caller.SessionID == "" || caller.ClientID == "" {
		return ""
	}
	return caller.SessionID + ":" + caller.ClientID
}

// productionPromptError keeps this read's failures symbolic, exactly like the
// transport binder's own mapper (promptskills_read.go:143-176): the prompt root
// reader can wrap an absolute path, a username or a raw os error, so only the
// finite vocabulary below is ever reflected to the browser.
func productionPromptError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("PROMPT_SKILLS_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "GLOBAL_DATA_NOT_READY":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	// These sentinels are fixed symbols (files_linux.go:18-20, service.go:20);
	// they never carry a path or a raw error.
	for _, known := range []error{promptskills.ErrUnsafe, promptskills.ErrChanged, promptskills.ErrUnsupported, promptskills.ErrClosed} {
		if errors.Is(err, known) {
			return errors.New(known.Error())
		}
	}
	if err != nil {
		switch err.Error() {
		case "PROMPT_SKILLS_ROOT_UNAVAILABLE", "PROMPT_SKILLS_PROVIDER_UNAVAILABLE":
			return errors.New(err.Error())
		}
	}
	return errors.New("PROMPT_SKILLS_READ_FAILED")
}
