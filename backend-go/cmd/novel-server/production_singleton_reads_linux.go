//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's FIVE-name read slice taken one name at a
// time, because every family binder that owns these names is atomic with a write
// and would hand this lane a mutation it does not own:
//
//	db:review-get-latest   internal/transport/reviewrevision.go:87-89 publishes
//	                       all fourteen review/revision names at once
//	                       (reviewRevisionOperations:69-75), six of which mutate
//	                       - create (:31), revision-create (:36),
//	                       revision-replace-pending (:37), revision-merge (:42),
//	                       revision-mark-merged (:43), revision-mark-discarded
//	                       (:44). The read-only remainder binder exists but is
//	                       the WRITE set (reviewRevisionRemainderOperations:80-84),
//	                       so BindReviewRevisionRemainder is not usable here: it
//	                       would publish the six writes and nothing else.
//	db:review-next-index   same family, same seat: :35 in the handler switch.
//	db:revision-next-index same family, same seat: :41.
//
//	db:recovery-candidate-list  internal/transport/recovery.go:41-45 is atomic
//	                       across the four candidate names, and three of them
//	                       mutate - record (:33), update (:34) and resolve
//	                       (:35) - so BindRecovery would publish a write surface.
//
//	skills:list-user       internal/transport/promptskills_read.go:51-57 publishes
//	                       prompt:load-global and skills:list-user together. The
//	                       lane already binds prompt:load-global itself
//	                       (production_open_linux.go:580 through
//	                       newProductionPromptLoad, production_chapter_reads_linux.go:108),
//	                       so taking BindPromptSkillsReads would fail startup with
//	                       OPERATION_ALREADY_BOUND; the disjoint write binder
//	                       (BindPromptSkillsWrites) is the other three names and is
//	                       not wanted here.
//
// Each name is therefore installed with server.Bind, the exported single-name
// binder (internal/transport/server.go:73-77), exactly the way
// production_importrun_reads_linux.go:164-178 and
// production_fs_reads_linux.go:205-222 install their own slices. server.Bind
// refuses a name that is not inventoried (OPERATION_NOT_EXPOSED) or is already
// bound (OPERATION_ALREADY_BOUND), so a name this file should not publish is a
// startup error instead of silent dead code.
//
// # Every bound store method is genuinely read-only
//
//  1. store.ReviewRevisions.LatestReview (internal/store/reviewrevision.go:153-157)
//     is ONE rrMaps SELECT over reviews LEFT JOIN contents (:101-119). It does not
//     call immediate(), so no transaction is opened at all.
//  2. store.ReviewRevisions.NextReviewIndex (:178-183) is one
//     `SELECT MAX(review_index)` through QueryRowContext on the borrowed *sql.DB.
//  3. store.ReviewRevisions.NextRevisionIndex (:185-190) is the same SELECT on
//     revisions.
//  4. store.Recovery.ListPending (internal/store/recovery.go:356-386) DOES open
//     BEGIN IMMEDIATE - it borrows
//     store.ReviewRevisions.immediate (reviewrevision.go:41-61, BEGIN at :46) to
//     hold one consistent snapshot across the candidate rows - and the closure it
//     runs contains no INSERT, UPDATE or DELETE: one
//     `SELECT * FROM recovery_candidates WHERE status='pending'` (:363-365) plus
//     store.recoveryCandidate (:192-207) and store.recoveryCurrent (:158-190),
//     which are two more SELECTs (`SELECT id,version FROM drafts ...`). The
//     only ExecContext calls in the whole file are the insert of Record (:334),
//     the update of UpdatePending (:410) and the update of Resolve (:466), and
//     none of them is reachable from ListPending. This is the same precedent the
//     already-bound summary reads use: a reservation that writes nothing, taken
//     while this process holds the exclusive legacy writer lease, so it cannot
//     block. A read therefore commits nothing, and the borrow's release closure
//     is a re-verification rather than a write acknowledgement.
//  5. promptskills.Service.ListUser (internal/promptskills/service.go:48-51) opens
//     the pinned private root's `skills` child with create=false, lists it, and
//     reads each `SKILL.md` plus its bundle manifest; openChildDir/readAt never
//     create, write or remove.

const (
	productionSingletonReviewLatestName = "db:review-get-latest"
	productionSingletonReviewNextName   = "db:review-next-index"
	productionSingletonRevisionNextName = "db:revision-next-index"
	productionSingletonRecoveryListName = "db:recovery-candidate-list"
	productionSingletonSkillsListName   = "skills:list-user"

	// The review/revision wire shape the family binder reads
	// (internal/transport/reviewrevision.go:23): arity 3, the scalar id first, the
	// frozen project path second, the three-key context LAST.
	productionSingletonReviewArity   = 3
	productionSingletonReviewIDPos   = 0
	productionSingletonReviewPathPos = 1

	// db:recovery-candidate-list is the family's one arity-2 name
	// (internal/transport/recovery.go:41-45): the frozen project path first, the
	// three-key context LAST.
	productionSingletonRecoveryArity   = 2
	productionSingletonRecoveryPathPos = 0

	// skills:list-user takes ZERO arguments (internal/transport/promptskills_read.go:56;
	// the renderer invokes it with no arguments at all, src/services/agent/
	// skill-registry.ts:132), so there is no path argument and no frozen lease to
	// parse.
	productionSingletonSkillsArity = 0
)

// productionSingletonReads is the adapter for the five names: one borrow of the
// admitted author database per database read, and the lane's own pinned global
// root for the installed-skill listing. It is stateless per request, so a lane
// opened after a refused startup answers the symbolic refusals instead of holding
// a handle this process no longer owns.
type productionSingletonReads struct {
	// durable is the author runtime bridge every database name borrows through.
	durable *productionDurableOpen
	// skills reuses the lane's own prompt:load-global seam: the pinned global
	// admission resolves the committed data root from its receipt, and that same
	// root is where the installed skills live. It resolves nothing at bind time.
	skills *productionPromptLoad
}

// newProductionSingletonReads returns the read adapter for the lane built by
// productionHandlerWithContract. Nothing is resolved or opened here.
func newProductionSingletonReads(open *productionOpen) *productionSingletonReads {
	if open == nil {
		return &productionSingletonReads{}
	}
	return &productionSingletonReads{
		durable: open.durable,
		skills:  &productionPromptLoad{global: open.global, startup: open.startup},
	}
}

// Bind installs the five reads, one server.Bind per name. A nil adapter is
// refused instead of being published as handlers that can only fail, and each
// name is re-checked against the registry by Bind itself.
func (r *productionSingletonReads) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil || r.skills == nil || server == nil {
		return errors.New("PRODUCTION_SINGLETON_BINDER_INVALID")
	}
	for _, bind := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionSingletonReviewLatestName, r.reviewRead(productionSingletonReviewLatestName)},
		{productionSingletonReviewNextName, r.reviewRead(productionSingletonReviewNextName)},
		{productionSingletonRevisionNextName, r.reviewRead(productionSingletonRevisionNextName)},
		{productionSingletonRecoveryListName, r.recoveryList},
		{productionSingletonSkillsListName, r.skillsList},
	} {
		if err := server.Bind(bind.name, bind.handler); err != nil {
			return err
		}
	}
	return nil
}

// reviewRead serves db:review-get-latest, db:review-next-index and
// db:revision-next-index: the three review/revision reads the existing
// ReviewRevisionProvider binder serves but whose family binder also carries the
// six writes. Argument order, the arity check and every refusal code are copied
// from that binder (:23-24), so the renderer sees one contract whichever binder
// answers it. The borrow is borrowAuthorDatabase - the identical fence the draft,
// character, stage, blueprint, continuity and review-list reads already use - so
// a stale or foreign lease is refused before any SQL runs, and store.NewReviewRevisions
// is exactly the repository the family binder constructs over the same handle
// (internal/transport/reviewrevision.go:28).
func (r *productionSingletonReads) reviewRead(operation string) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("PRODUCTION_SINGLETON_UNAVAILABLE")
		}
		if len(args) != productionSingletonReviewArity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[productionSingletonReviewArity-1])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		// The family binder requires args[1] to BE the frozen project path
		// (:24): a page cannot aim one project's lease at another project.
		path, ok := args[productionSingletonReviewPathPos].(string)
		if !ok || path == "" {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected.ProjectPath {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// The scalar id is read exactly like draftSafeInteger(args[0], 1)
		// (internal/transport/drafts.go:183): a whole JavaScript-safe number, no
		// string coercion, never below 1.
		id, ok := productionImportRunReadInteger(args[productionSingletonReviewIDPos])
		if !ok || id < 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[productionSingletonReviewArity-1])
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionSingletonReviewFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("PRODUCTION_DURABLE_INVALID")
		}
		repository := store.NewReviewRevisions(db)
		var result any
		switch operation {
		case productionSingletonReviewLatestName:
			row, readErr := repository.LatestReview(ctx, id)
			if readErr != nil {
				return nil, productionSingletonReviewFailure(readErr)
			}
			// store.ReviewRevisionFullWire is the same projection the binder's
			// reviewRevisionWire applies (:66), and a missing row stays the
			// original's nullable null instead of becoming an error.
			result = store.ReviewRevisionFullWire(row)
		case productionSingletonReviewNextName:
			result, err = repository.NextReviewIndex(ctx, id)
		case productionSingletonRevisionNextName:
			result, err = repository.NextRevisionIndex(ctx, id)
		default:
			return nil, errors.New("PRODUCTION_SINGLETON_OPERATION_UNKNOWN")
		}
		if err != nil {
			return nil, productionSingletonReviewFailure(err)
		}
		return result, nil
	}
}

// recoveryList serves db:recovery-candidate-list: the pending recovery candidates
// the author's own database holds, in the family's exact envelope
// (internal/transport/recovery.go:120-132) - a 200 with the candidate array, an
// empty array when nothing is pending, and a transport refusal otherwise. Its
// BEGIN IMMEDIATE reservation writes nothing; see the header.
func (r *productionSingletonReads) recoveryList(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if r == nil || r.durable == nil {
		return nil, errors.New("PRODUCTION_SINGLETON_UNAVAILABLE")
	}
	if len(args) != productionSingletonRecoveryArity {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := projectsession.ParseContext(args[productionSingletonRecoveryArity-1])
	if err != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	path, ok := args[productionSingletonRecoveryPathPos].(string)
	if !ok || path == "" || !utf8.ValidString(path) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected.ProjectPath {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[productionSingletonRecoveryArity-1])
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionSingletonRecoveryFailure(err)
	}
	if db == nil || release == nil {
		return nil, errors.New("PRODUCTION_DURABLE_INVALID")
	}
	candidates, err := store.NewRecovery(db).ListPending(ctx)
	if err != nil {
		return nil, productionSingletonRecoveryFailure(err)
	}
	if candidates == nil {
		// The family binder answers an empty array, never null, for a project
		// with no pending candidate (:120-132).
		candidates = []map[string]any{}
	}
	return candidates, nil
}

// skillsList serves skills:list-user from the same pinned global root the lane
// already serves prompt:load-global from. Host paths are not browser
// capabilities: the projection below is the family binder's own
// (internal/transport/promptskills_read.go:120-142), which replaces every skill's
// absolute BaseDir/FilePath with a stable managed:// display identifier, so the
// private root, the username and the os error text never travel.
func (r *productionSingletonReads) skillsList(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if r == nil || r.skills == nil {
		return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	if len(args) != productionSingletonSkillsArity {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	// The family's caller gate, with the startup readiness the lane's own
	// prompt:load-global already requires (production_chapter_reads_linux.go:122-128):
	// an unidentified caller, or a generation that is not committed, never
	// reaches the private root.
	if key := productionGlobalCaller(caller); key == "" ||
		r.skills.startup == nil || r.skills.startup.State(key, true).State != "ready" {
		return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
	}
	if ctx == nil {
		return nil, errors.New("PROMPT_SKILLS_READ_FAILED")
	}
	root, err := r.skills.pinnedRoot()
	if err != nil {
		return nil, productionPromptError(err)
	}
	// A fresh pin per request: promptskills.New re-opens every ancestor without
	// following a symlink and re-checks the owner and the mode, and the
	// descriptor is released before the response is written.
	service, err := promptskills.New(root)
	if err != nil {
		return nil, productionPromptError(err)
	}
	defer func() { _ = service.Close() }()
	skills, err := service.ListUser()
	if err != nil {
		return nil, productionPromptError(err)
	}
	return productionSingletonSkillWire(skills), nil
}

// productionSingletonSkillWire is this file's copy of the transport binder's
// installed-skill projection (internal/transport/promptskills_read.go:120-142).
// UserSkill.BaseDir and UserSkill.FilePath carry the private absolute root, so
// they are never forwarded: each skill is addressed through managed:// instead.
func productionSingletonSkillWire(skills []promptskills.UserSkill) []any {
	public := make([]any, 0, len(skills))
	for _, skill := range skills {
		bundle := map[string]any{"status": skill.Bundle.Status}
		if skill.Bundle.Status == "intact" {
			files := make([]any, 0, len(skill.Bundle.Files))
			for _, file := range skill.Bundle.Files {
				files = append(files, map[string]any{"path": file.Path, "content": file.Content})
			}
			bundle["files"] = files
		}
		base := "managed://skills/" + skill.Name
		public = append(public, map[string]any{
			"name":     skill.Name,
			"content":  skill.Content,
			"baseDir":  base,
			"filePath": base + "/SKILL.md",
			"bundle":   bundle,
		})
	}
	return public
}

// productionSingletonSymbolicCode bounds the vocabulary a repository refusal may
// surface: an uppercase symbolic constant, never a wrapped path or a raw SQLite
// sentence. It is the same expression the family binders' mappers rely on
// (internal/transport/recovery.go:259-289, production_importrun_reads_linux.go:258).
var productionSingletonSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productionSingletonReviewFailure keeps a review/revision refusal symbolic,
// exactly like the family binder's non-mutating branch (which returns the
// registry's HTTPError untouched and everything else as reviewRevisionCode,
// internal/transport/reviewrevision.go:22, :52-55).
func productionSingletonReviewFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if productionSingletonSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("REVIEW_REVISION_OPERATION_FAILED")
}

// productionSingletonRecoveryFailure is the recovery family's mapper
// (internal/transport/recovery.go:259-289): a cancellation keeps its own code, a
// public HTTP error is rebuilt rather than forwarded verbatim, a symbolic
// repository code survives, and any other text - raw SQLite detail included - is
// replaced by the family's fallback.
func productionSingletonRecoveryFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return security.NewHTTPError(public.Status, public.Code)
	}
	if productionSingletonSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("RECOVERY_CANDIDATE_OPERATION_FAILED")
}

// # Refused: fs:list-dir, and why it cannot be rescued by a confinement wrapper
//
// fs:list-dir stays unbound on this lane, so it keeps failing closed at
// 501 GO_OPERATION_NOT_MIGRATED. The three places that decide this are:
//
//   - internal/transport/editor_nav_reads.go:60-92, the family handler, takes an
//     ABSOLUTE request-supplied host directory and derives a relative one with
//     filepath.Rel (:80-83). That is the lexical containment comparison
//     internal/projectfs/browser_files.go:38-52 says a browser-facing server must
//     never depend on: one missing comparison addresses any path the server user
//     can reach.
//   - internal/projectfs/read_tree.go:34-80 does offer a relative-path entry
//     point (ListVisibleTree), but its projection still hands the page an
//     ABSOLUTE host path per node: `node := VisibleNode{Name: name, Path:
//     filepath.Join(p.path, childRelative)}` (:132), with VisibleNode.Path
//     documented as the browser's fs:list-dir shape (:21-27). Confining the
//     request therefore does not confine the RESPONSE: the caller would learn the
//     server's private root and every child of it.
//   - production_editor_reads_linux_test.go:420 and
//     production_fs_reads_linux_test.go:270-288 already pin the name at 501 for
//     exactly this reason.
//
// Rescuing it would mean writing a second path walk and a rewritten projection in
// package main, over a store method that already publishes host paths; inventing
// that for one listing operation is not a read this lane can answer honestly, so
// the name is refused here and no handler is installed for it.
