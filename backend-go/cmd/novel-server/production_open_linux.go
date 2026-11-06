//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globaladmission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// NOVEL_DATA_ROOT is the author data mount the shipped web gateway already uses.
// Every path below is derived from it in the same way, so a data directory the
// existing gateway wrote is admitted unchanged.
// This lane never creates a project directory, a database or a migration
// journal: it creates the persistent writer-lease entry the shipped gateway also
// creates, the private scratch workspace it owns, and it writes author data only
// through the admitted author runtime while that lease is held.
const (
	productionLibraryDirectory = "workspaces"
	productionConfigDirectory  = "config"
	productionConfigTarget     = "ai-novel-writer"
	productionMigrationTarget  = "ai-novel-writer-web-runtime"
	productionLegacyDirectory  = "legacy-global"
	// productionMaxDatabaseBytes bounds one staged author database copy. It is
	// the same 64 MiB budget the durable synthetic lane admits.
	productionMaxDatabaseBytes = 64 << 20
	// productionStartupTimeout bounds the whole startup admission, including one
	// staged copy per library project.
	productionStartupTimeout = 5 * time.Minute
	// productionRecentLimit matches the shipped recent-project shelf bound.
	productionRecentLimit = 20
	// productionLibraryEntries matches the browser chooser bound.
	productionLibraryEntries = 4096
)

// productionLayout is the trusted physical layout of one author data root.
// No component is request-derived; each is a fixed child of NOVEL_DATA_ROOT.
type productionLayout struct {
	dataRoot        string
	libraryRoot     string
	canonicalTarget string
	migrationRoot   string
	legacySource    string
}

func productionLayoutFor(raw string) (productionLayout, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsRune(value, '\x00') || !filepath.IsAbs(value) {
		return productionLayout{}, errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	// Operators pass a configured mount, so normalising a trailing separator is
	// not authority: nothing here is a browser-supplied path.
	root := filepath.Clean(value)
	if root == string(filepath.Separator) {
		return productionLayout{}, errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	return productionLayout{
		dataRoot:        root,
		libraryRoot:     filepath.Join(root, productionLibraryDirectory),
		canonicalTarget: filepath.Join(root, productionConfigDirectory, productionConfigTarget),
		migrationRoot:   filepath.Join(root, productionConfigDirectory, productionMigrationTarget),
		legacySource:    filepath.Join(root, productionLegacyDirectory),
	}, nil
}

// productionConfig is the trusted backend input of this lane. Schema is the
// shipped pinned v7 admission contract; a test substitutes a synthetic one so
// the staging wiring can be proven without the TypeScript schema oracle.
type productionConfig struct {
	Schema           admission.SchemaContract
	MaxDatabaseBytes int64
	// Dispatch is the generation executor's injectable dispatch seam. It is
	// synthetic-only: production main.go leaves it nil, so a real dispatch can
	// only leave through the committed model-egress policy. A test that sets it
	// feeds a deterministic provider response instead of dialing, which is why it
	// is wired into the executor in the same place the lane is built.
	Dispatch generation.Dispatch
}

// productionProject is one library entry whose database was admitted as a
// current-schema author database, with the evidence proving it was read.
type productionProject struct {
	listed   projectfs.ListedProject
	evidence admission.Evidence
}

// productionOpen is the production lane. It pins the completed global data
// migration, owns the author library under one exclusive writer lease, and
// answers the operations that read real state (startup/config from the committed
// generation, the recent shelf, the library chooser) plus the project lifecycle
// and the draft reads/writes the durable author runtime serves on the author's
// own database.
//
// Every admitted project is openable through the runtime; everything this build
// does not implement (creation, import, generation, the remaining editor reads)
// still fails closed at GO_OPERATION_NOT_MIGRATED.
type productionOpen struct {
	mu       sync.Mutex
	layout   productionLayout
	global   *globaladmission.PinnedReady
	config   *globalconfig.Service
	startup  *globalconfig.Startup
	library  *projectfs.Library
	scratch  string
	projects []productionProject
	// held is the exclusive legacy writer lease this process took before
	// touching any author database. It is nil once the durable runtime owns it.
	held *writerlock.Lock
	// durable is the author runtime plus its transport adapter. It is nil only
	// while startup is still admitting projects, or after Close.
	durable *productionDurableOpen
	// modelEgress is the committed model-egress policy the generation executor
	// dispatches through. It is nil when the committed config names no usable
	// origin; modelEgressErr then keeps the symbolic refusal (never a committed
	// value) for diagnostics. Granting the policy is not granting generation by
	// itself: it is one input to the executor the generation owner builds, and a
	// nil policy leaves that executor unbuilt so generation:execute refuses.
	modelEgress    *productionModelEgress
	modelEgressErr error
	closed         bool
}

// openProduction admits a real author data directory, or fails closed with a
// symbolic code. A directory that is not a completed global migration, or not
// an author library, is never repaired, seeded or created here.
func openProduction(ctx context.Context, layout productionLayout, config productionConfig) (*productionOpen, error) {
	if ctx == nil || len(config.Schema) == 0 || config.MaxDatabaseBytes <= 0 {
		return nil, errors.New("PRODUCTION_CONFIGURATION_INVALID")
	}
	if err := requirePhysicalDirectory(layout.dataRoot); err != nil {
		return nil, err
	}
	// One cooperating writer per data root. This takes the same util-linux flock
	// entry the shipped gateway uses, before any author database is read or
	// written, and holds it for the process lifetime: a second server on this
	// mount is refused here instead of racing this one. The Go image entrypoint
	// runs the binary directly, so the lease is acquired rather than inherited;
	// a container that already holds this entry (an outer flock) is refused.
	held, err := writerlock.AcquireLegacy(layout.dataRoot)
	if err != nil {
		return nil, fmt.Errorf("production writer lease: %w", err)
	}
	// exclusive is a trusted claim made by main; no request field can reach it.
	global, err := globaladmission.PinCompleted(ctx, globaladmission.Roots{
		LegacySource:    layout.legacySource,
		CanonicalTarget: layout.canonicalTarget,
		UserData:        layout.migrationRoot,
	}, true)
	if err != nil {
		_ = held.Close()
		return nil, fmt.Errorf("production global data: %w", err)
	}
	open := &productionOpen{layout: layout, global: global, held: held}
	fail := func(cause error) (*productionOpen, error) {
		_ = open.Close()
		return nil, cause
	}
	// PinCompleted already verified the migration semantics; that identity read
	// returns the receipt facts (committed generation, ignored legacy source,
	// preserved unknown count) the startup state must report.
	ready, err := global.VerifyIdentity(ctx)
	if err != nil {
		return fail(fmt.Errorf("production global data: %w", err))
	}
	open.config, err = globalconfig.New(filepath.Clean(ready.DataRoot))
	if err != nil {
		return fail(fmt.Errorf("production global config: %w", err))
	}
	// The startup capture mirrors the shipped gateway exactly: the generation is
	// the committed receipt generation, never a request value or a directory
	// name, and the notice reports the migration facts the receipt proves.
	open.startup = globalconfig.NewStartup(globalconfig.StartupCapture{
		Generation: ready.GlobalGeneration,
		MigrationNotice: &globalconfig.MigrationNotice{
			LegacySourceIgnored:   ready.LegacySourceIgnored,
			PreservedUnknownCount: ready.PreservedUnknownCount,
		},
	})
	// The model-egress policy the generation executor dispatches through is
	// derived once, here, from the committed global generation this lane already
	// pinned: the config document's explicit modelOrigins allowlist, else the
	// origins of the committed model endpoints. It grants no dispatch by itself -
	// no provider client or request is created, and the executor that uses it is
	// built only when a generation request arrives. An absent, unreadable or
	// malformed origin list is an optional provider field: it never fails startup
	// and never widens to default-allow. The refusal code is logged; the rejected
	// values are not.
	egressPlan, egressErr := productionModelEgressPlan(open.config)
	if egressErr != nil {
		open.modelEgressErr = egressErr
		log.Printf("novel-server: production model egress unavailable (%v); generation:execute will refuse", egressErr)
	} else {
		open.modelEgress = egressPlan
		log.Printf("novel-server: production model egress restricted to %d committed origin(s)", len(egressPlan.origins))
	}
	open.library, err = projectfs.OpenLibrary(layout.libraryRoot)
	if err != nil {
		return fail(fmt.Errorf("production library: %w", err))
	}
	open.scratch, err = os.MkdirTemp(os.TempDir(), "novel-production-admission-")
	if err != nil {
		return fail(err)
	}
	listed, err := open.library.ListCanonicalProjects(ctx, projectfs.MaxLibraryEntries)
	if err != nil {
		return fail(fmt.Errorf("production library inventory: %w", err))
	}
	admitted := make([]productionProject, 0, len(listed))
	var rejected error
	for _, entry := range listed {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		evidence, admitErr := admitProductionProject(ctx, open.library, entry, open.scratch, config)
		if admitErr != nil {
			if rejected == nil {
				rejected = admitErr
			}
			log.Printf("novel-server: not a current-schema author database, left unserved: %s: %v", entry.Path, admitErr)
			continue
		}
		admitted = append(admitted, productionProject{listed: entry, evidence: evidence})
	}
	// A library holding projects this build cannot admit is a configuration
	// failure, not an empty shelf: report it instead of hiding the author's data.
	if len(listed) > 0 && len(admitted) == 0 {
		return fail(fmt.Errorf("production library holds no admitted author database: %w", rejected))
	}
	open.projects = admitted
	log.Printf("novel-server: admitted %d of %d author projects from %s", len(admitted), len(listed), open.layout.libraryRoot)
	// The author runtime is installed last, over the already admitted library,
	// and takes over the writer lease this function has held since the top. A
	// failure here closes the runtime (and therefore the lease) before returning.
	durable, err := newProductionDurable(open.library, open.layout, open.scratch, held, config, admitted)
	if err != nil {
		if errors.Is(err, errProductionLeaseRetained) {
			// Never release a lease whose runtime could not close every handle.
			open.held = nil
		}
		return fail(fmt.Errorf("production author runtime: %w", err))
	}
	open.held = nil
	open.durable = durable
	return open, nil
}

// admitProductionProject proves one library entry is a current-schema author
// database without writing it and without leaving a handle open. It stages the
// exact database set into a private copy, exactly like projectaccess.Preflight,
// and then verifies that copy against the trusted contract. The author's own
// database is only ever byte-copied, never opened.
func admitProductionProject(ctx context.Context, library *projectfs.Library, entry projectfs.ListedProject, workspace string, config productionConfig) (admission.Evidence, error) {
	var zero admission.Evidence
	project, err := library.OpenProject(entry.Path)
	if err != nil {
		return zero, err
	}
	defer project.Close()
	if project.ProjectID() != entry.ProjectID {
		return zero, errors.New("PRODUCTION_PROJECT_IDENTITY_CHANGED")
	}
	directory, err := os.MkdirTemp(workspace, "admit-")
	if err != nil {
		return zero, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return zero, err
	}
	_, stageErr := project.StageDatabase(ctx, root, config.MaxDatabaseBytes)
	closeErr := root.Close()
	if stageErr != nil {
		return zero, stageErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	evidence, err := admission.VerifySnapshot(ctx, filepath.Join(directory, "snapshot.db"), config.Schema)
	if err != nil {
		return zero, err
	}
	return evidence, nil
}

func requirePhysicalDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("PRODUCTION_DATA_ROOT_UNSAFE")
	}
	return nil
}

func (p *productionOpen) closedError() error {
	if p == nil || p.closed {
		return errors.New("PRODUCTION_CLOSED")
	}
	return nil
}

// ListCanonicalProjects is the /api/library inventory. It serves only the
// projects whose author database was admitted at startup, so the chooser and
// the recent shelf can never disagree about what this build can read.
func (p *productionOpen) ListCanonicalProjects(ctx context.Context, maxEntries int) ([]projectfs.ListedProject, error) {
	if p == nil || ctx == nil || maxEntries < 1 || maxEntries > projectfs.MaxLibraryEntries {
		return nil, errors.New("PRODUCTION_LIBRARY_REQUEST_INVALID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.closedError(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listed := make([]projectfs.ListedProject, 0, len(p.projects))
	for _, item := range p.projects {
		if len(listed) >= maxEntries {
			break
		}
		listed = append(listed, item.listed)
	}
	return listed, nil
}

// RecentProjects is project:recent-list. The shipped gateway indexes the
// canonical library directly instead of the desktop recent-projects.json, so
// this shelf is the admitted library in the library's own newest-first order.
func (p *productionOpen) RecentProjects(ctx context.Context, _ transport.Caller) ([]transport.RecentNavigationProject, error) {
	if p == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_LIBRARY_REQUEST_INVALID")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.closedError(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]transport.RecentNavigationProject, 0, len(p.projects))
	for _, item := range p.projects {
		if len(result) >= productionRecentLimit {
			break
		}
		result = append(result, transport.RecentNavigationProject{
			Name: item.listed.Name, Path: item.listed.Path,
			UpdatedAt: item.listed.UpdatedAt, ProjectID: item.listed.ProjectID,
		})
	}
	return result, nil
}

// ModelEgress is the committed model-egress policy the generation executor
// dispatches through, and the gate generation:begin and generation:execute read
// before they touch a run. It is deliberately not reachable from any browser
// operation as a value: no operation returns it, and the only client that can use
// it is the executor's native dispatch. A nil client with a symbolic refusal
// means the committed config named no usable origin, which is never widened into
// a default-allow policy and never becomes a dispatch of its own.
func (p *productionOpen) ModelEgress() (*egress.Client, error) {
	if p == nil {
		return nil, errProductionModelEgressUnavailable
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.closedError(); err != nil {
		return nil, err
	}
	if p.modelEgress == nil || p.modelEgress.policy == nil {
		if p.modelEgressErr != nil {
			return nil, p.modelEgressErr
		}
		return nil, errProductionModelEgressUnconfigured
	}
	return p.modelEgress.policy, nil
}

func (p *productionOpen) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	workspace := p.scratch
	p.scratch = ""
	library, config, global := p.library, p.config, p.global
	durable, held := p.durable, p.held
	modelEgress := p.modelEgress
	p.modelEgress = nil
	p.mu.Unlock()
	var result error
	// The admitted runtime closes every open author database and then releases
	// the writer lease. A runtime that cannot confirm that keeps the lease until
	// the process exits instead of handing the data root to a second writer.
	if durable != nil {
		result = errors.Join(result, durable.Close())
	} else if held != nil {
		result = errors.Join(result, held.Close())
	}
	// Closing the policy only releases its idle connections; it holds no origin
	// state outside this process and performs no request on close.
	if modelEgress != nil {
		modelEgress.policy.Close()
	}
	if library != nil {
		result = errors.Join(result, library.Close())
	}
	if config != nil {
		result = errors.Join(result, config.Close())
	}
	if global != nil {
		result = errors.Join(result, global.Close())
	}
	if workspace != "" {
		result = errors.Join(result, os.RemoveAll(workspace))
	}
	return result
}

// productionHandler is the only startup path main uses. The schema authority is
// the shipped pinned v7 contract, never a caller-selected file.
func productionHandler(registry *policy.Registry, address, staticDir, dataRoot string) (http.Handler, func(), error) {
	contract, err := admission.PinnedSchemaContract()
	if err != nil {
		return nil, nil, fmt.Errorf("production schema contract: %w", err)
	}
	return productionHandlerWithContract(registry, address, staticDir, dataRoot, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
}

// productionHandlerWithContract is the lane's test seam: it exists so a
// synthetic tiny database can prove the staging and verification wiring even
// though its schema fingerprint is not in the shipped contract.
func productionHandlerWithContract(registry *policy.Registry, address, staticDir, dataRoot string, config productionConfig) (http.Handler, func(), error) {
	layout, err := productionLayoutFor(dataRoot)
	if err != nil {
		return nil, nil, err
	}
	startupContext, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startupContext, layout, config)
	if err != nil {
		return nil, nil, err
	}
	server := transport.New(registry, security.NewOrigins("http://"+address), "go-production-admitted")
	if bindErr := server.BindNavigation(); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	// A real author data directory still serves only the read and acknowledgement
	// half of the global config family. config:set would write the author's
	// committed generation and keeps failing closed at GO_OPERATION_NOT_MIGRATED
	// until a lane owns that write.
	if bindErr := server.BindGlobalConfigReads(open.config, open.startup); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	// The project lifecycle over the durable author runtime: project:open,
	// db:close, project:get-runtime-context, project:save and project:update-config.
	// BindProjects is the only binder that publishes project:open; it also binds
	// project:recent-list and project:recent-remove only when the provider
	// implements the optional catalog interfaces, which productionDurableOpen
	// does not — the shelf stays with the explicitly bound handler below.
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	// The draft surface, in three disjoint binders: the eight live draft reads,
	// the single full-body CAS save, and the three self-contained author
	// mutations (create, update-status, delete). Nothing here overlaps the
	// lifecycle names above, so no operation is ever double-bound.
	for _, bind := range []func() error{
		func() error { return server.BindDraftLiveReads(open.durable) },
		func() error { return server.BindDraftLiveWrites(open.durable) },
		func() error { return server.BindDraftAuthorWrites(open.durable) },
	} {
		if bindErr := bind(); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
	}
	// The editor's read surface, bound to the same admitted author database under
	// the same live lease: the three character reads, the two stage reads
	// (project core and the whole blueprint list the chapter/volume tree is built
	// from), the single blueprint read, and the project overview aggregate. Each
	// one is a SELECT-only path; nothing here grants a write, and these four
	// distinct binders never overlap, so no operation is double-bound.
	for _, bind := range []func() error{
		func() error { return server.BindCharacterLiveReads(open.durable) },
		func() error { return server.BindEditorStageReads(open.durable) },
		func() error { return server.BindBlueprintGetRead(open.durable) },
		func() error { return server.BindCurrentOverview(open.durable) },
	} {
		if bindErr := bind(); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
	}
	// The generation family's committed-state half plus the model-configuration
	// reads. generation:list and generation:read project the author's own ledger
	// rows under the live lease (SELECT-only, no provider call), and the four
	// llm reads answer from the committed global generation this lane already
	// pinned. Every other dispatch name stays unbound, so resume/restart/pause and
	// the six llm setters keep failing closed at GO_OPERATION_NOT_MIGRATED rather
	// than being installed as handlers that could only refuse.
	//
	// This lane owns exactly two generation names, both through the one reduced
	// owner the verified synthetic lane implements
	// (production_generation_begin_linux.go): generation:begin opens a ledger run
	// under the live lease from the committed config and writes the root budget row
	// and the run row, and generation:execute dispatches that admitted run through
	// the generationowner.Executor, whose sole network client is the committed
	// model-egress policy built above. The executor's dispatch seam is nil unless
	// this lane was opened with the synthetic one, so a real deployment cannot dial
	// past the allowlist.
	if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {
		if bindErr := server.BindGenerationBegin(&transport.GenerationProvider{Begin: owner.Begin}); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// graph-generation:begin freezes the plan context of one graph run and
		// opens its ledger row through the same reduced admission
		// (production_generation_begin_linux.go BeginGraph). It is the only graph
		// name this lane serves: execute, confirm and the handle read stay unbound,
		// so a begun run is recoverable by replaying its nonce and cannot yet be
		// dispatched or settled here.
		if bindErr := server.Bind("graph-generation:begin", productionGenerationMutation(1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return owner.BeginGraph(ctx, caller, expected, args[0])
		})); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// db:import-run-finalize-parsing, -restart, -fail and -effect-receipt-get
		// were unbound because the store had no lifecycle behind them. It does now
		// (internal/store/importrun_lifecycle.go), so those four are served here.
		// The other four import-run names stay 501: prepare-inspection and
		// author-preview need an inspection owner this lane does not have, and
		// receipt prepare/commit need a generation-source proof that does not
		// exist yet (importrun_receipts.go:112-118).
		if bindErr := newProductionImportRunResiduals(open.durable).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		if bindErr := server.BindGenerationExecute(&transport.GenerationProvider{Execute: owner.Execute}); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// generation:cancel, generation:pause and generation:resume settle or advance
		// an admitted run through the ledger and dial nothing, so the same owner lends
		// them under the same committed gates.
		if bindErr := server.BindGenerationLifecycle(&transport.GenerationProvider{Cancel: owner.Cancel, Pause: owner.Pause, Resume: owner.Resume}); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// The remaining ledger mutations of the family - material decisions, candidate
		// discards, restart and the two family cancels - travel the same owner, because
		// none of them dials: restart runs the begin body and then stops the old root,
		// so it is not a second run-admission path. retry-draft-short-outline is absent:
		// it synthesises a task the browser never froze and re-dispatches it.
		if bindErr := newProductionGenerationWrites(owner).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// generation:prepare-draft-context and generation:read-context project the
		// chapter-draft context; both dial nothing and neither writes the author
		// database. generation:commit-draft stays unbound: its composed text and
		// unit-count ports do not exist in Go yet.
		if bindErr := newProductionDraftContext(owner).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// generation:commit-draft recomposes the visible text from the run's own
		// artifacts and commits it in one immediate transaction, with the
		// next-chapter guard inside that transaction.
		if bindErr := newProductionDraftCommit(owner).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// generation:compose-visible stores the visible composition receipt on the
		// attempt that composed it; generation:read-visible-composition reads it
		// back and re-verifies its hash. Neither dials a provider.
		if bindErr := newProductionComposition(owner).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
		// generation:begin-batch opens the batch root and its run; it validates the
		// intent before admission and dispatches nothing.
		if bindErr := newProductionBatchWrite(owner).Bind(server); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
	} else {
		log.Printf("novel-server: production generation owner unavailable (%v); generation:begin and generation:execute stay unbound", ownerErr)
	}
	// The continuity, post-process ledger and plot-tree halves of the editor's
	// read surface, borrowed from the same admitted author database under the same
	// live lease as everything above (production_continuity_reads_linux.go). The
	// three summary reads project the author's own snapshot rows under a
	// BEGIN IMMEDIATE reservation that writes nothing, the five post-process
	// receipt / llm ledger reads are plain SELECTs, and db:plot-tree-read opens
	// its own read-only transaction over the project core, blueprint, finalized
	// chapter and narrative rows. BindSummary also installs the one legacy
	// snapshot INSERT (db:save-summary-snapshot) and keeps the two continuity
	// names refusing at the binder's own
	// GENERATION_FINALIZATION_ADMISSION_REQUIRED. The four mutating post-process
	// names and db:plot-tree-save/db:plot-tree-clear are not bound here, so they
	// keep failing closed at GO_OPERATION_NOT_MIGRATED.
	for _, bind := range []func() error{
		func() error { return newProductionGraphReads(open.durable).Bind(server) },
		func() error { return server.BindGenerationReads(newProductionGenerationReads(open.durable)) },
		func() error { return newProductionBatchReads(open.durable).Bind(server) },
		func() error { return bindProductionFinalizationGeneration(server) },
		func() error { return server.BindModelReads(open.config, open.startup) },
		func() error { return server.BindModelRoutingReads(open.config, open.startup) },
		func() error { return server.BindSummary(open.durable) },
		func() error { return server.BindPostHistoryReads(open.durable) },
		func() error { return server.BindPlotTreeReads(open.durable) },
		func() error { return server.Bind("prompt:load-global", newProductionPromptLoad(open.global, open.startup)) },
		func() error { return server.BindEditorReviewReads(newProductionReviewReads(open.durable)) },
		func() error { return server.BindEditorReviewFullReads(newProductionReviewReads(open.durable)) },
		func() error { return server.BindEditorReviewCycleAbsentRead(newProductionReviewReads(open.durable)) },
		func() error { return newProductionImportRunReads(open.durable).Bind(server) },
		func() error { return newProductionImportRunWrites(open.durable).Bind(server) },
		func() error { return newProductionFSReads(open).Bind(server) },
		func() error { return newProductionGlobalWrites(open).Bind(server) },
		func() error { return newProductionFileWrites(open).Bind(server) },
		func() error { return newProductionSingletonReads(open).Bind(server) },
		func() error { return server.BindDraftAuthorityReads(newProductionDraftAuthorityReads(open.durable)) },
		func() error { return server.BindNarrativeReads(newProductionNarrativeReads(open.durable)) },
		func() error { return server.BindPostHistoryWrites(newProductionStoryStateWrites(open.durable)) },
		func() error { return server.BindPlotTreeClear(newProductionStoryStateWrites(open.durable)) },
		func() error { return server.BindNarrativeWrites(newProductionStoryStateWrites(open.durable)) },
		func() error { return server.BindNarrativeEventConfirm(newProductionStoryStateWrites(open.durable)) },
		func() error { return newProductionWrites(open).Bind(server) },
		func() error { return newProductionEditorWrites(open.durable).Bind(server) },
		func() error { return newProductionFinalizationWrites(open.durable).Bind(server) },
		// finalized-character:read-context is the finalization family's first
		// served name: it freezes the context a later commit re-proves. The store
		// port behind it landed just before this binder, so the answer is a real
		// read of the admitted author database rather than a restatement of the
		// request. Its five siblings stay 501 - commit needs the state-response
		// parser, and the three candidate operations need the candidate store.
		func() error { return newProductionFinalizedCharacterReads(open.durable).Bind(server) },
		// The three character-state candidate names complete the finalization
		// family's read side: an author sees what the just-finalized chapter wants
		// to change in the roster, and accepts or declines each field. commit stays
		// 501 - it needs the state-response parser, which does not exist yet.
		func() error { return newProductionCharacterStateCandidates(open.durable).Bind(server) },
		func() error { return newProductionProjectLifecycle(open).Bind(server) },
		func() error { return newProductionProjectCreate(open, config).Bind(server) },
	} {
		if bindErr := bind(); bindErr != nil {
			_ = open.Close()
			return nil, nil, bindErr
		}
	}
	// project:recent-list is bound on its own: the editor navigation family would
	// also publish fs:list-dir (which has no confined production provider yet)
	// and would rebind project:recent-list (OPERATION_ALREADY_BOUND).
	if bindErr := server.Bind("project:recent-list", func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		return open.RecentProjects(ctx, caller)
	}); bindErr != nil {
		_ = open.Close()
		return nil, nil, bindErr
	}
	route, err := transport.NewLibraryRoute(server, open, productionLibraryEntries)
	if err != nil {
		_ = open.Close()
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/library", route)
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	var static *transport.StaticAssets
	cleanup := func() {
		server.Close()
		if static != nil {
			_ = static.Close()
		}
		_ = open.Close()
	}
	if staticDir != "" {
		static, err = transport.NewStaticAssets(staticDir, server.SessionStore())
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("static assets: %w", err)
		}
		mux.Handle("/", static)
	} else {
		mux.Handle("/", server)
	}
	return mux, cleanup, nil
}
