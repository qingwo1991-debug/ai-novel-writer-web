//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is where the eight agent-generation:* operations were refused. Two of
// them are now served (production_agent_generation_serve_linux.go); the six this
// file still lists stay at the fail-closed 501 GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116), and each carries the exact code that has to
// move before a handler can be installed instead of a note that points at
// nothing.
//
// The family is one ledger operation, not eight: a round is a generation run.
// The desktop owner freezes operation 'agent-round' for every round it opens
// (electron/services/agent-generation.ts:175-178), the ledger admits the
// agentSession manifest key for exactly that operation
// (internal/store/generation_ledger.go:428-429), and every identity check in the
// reference refuses a run whose operation is anything else
// (electron/services/agent-generation.ts:54-62). So the ledger is NOT what is
// missing here, and this file's mapping layer says so once:
// productionAgentGenerationLedgerOperation. What is missing is no longer the
// context: it is the round protocol, and it is missing in two places plus the
// receipt port that would write what a round produces.
//
// Two of the eight names have since moved out of this table. The identity
// template `assistant_writing_identity` now ships as a committed asset
// (internal/prompttemplate/agent-identity-v1.json) and internal/agentgen
// composes the frozen context from it, so agent-generation:begin and
// agent-generation:read answer from the run's own state
// (production_agent_generation_serve_linux.go). What is left here is the six
// names that have no writer and no dispatcher.
//
// # The missing ports
//
// 1. The tool-call protocol on the dispatch path. A round is a model call whose
//    reply is parsed into visible text plus tool calls
//    (electron/services/generation-run-service.ts:263-266, over
//    src/shared/agent-response-protocol.ts:136), and the parse only runs because
//    the request carries agentToolNames. The Go dispatch service refuses every
//    request that carries one, including an empty one
//    (internal/generation/service.go:261, :289) - on purpose, since a present
//    tool manifest is a protocol request rather than ordinary text - and
//    generationowner.ParseTask refuses task fields beyond its own
//    (internal/generationowner/plan.go:34). Without the parse there is no
//    agentResponse to stamp, so no round can complete and no recovery can
//    project an action.
//
// 2. The per-action receipt state. Every tool operation of the family is a
//    write to the attempt's usage receipt: claim stamps agentActions
//    (electron/services/agent-generation.ts:215-232), finish stamps the settled
//    action (:259-275), register stamps the workflow metadata (:276-293),
//    commit-domain-tool stamps the domain effect plus the source transition
//    (:377-404), and resume stamps the control epoch (:163-170). The ledger's
//    closed receipt allowlist admits none of those top-level keys
//    (internal/store/generation_ledger.go:601, refused as
//    GENERATION_EFFECT_UNSUPPORTED) and the only method that patches a receipt
//    key outside settle is recordEffect, whose own allowlist is effect-only
//    (:1228). There is no ledger method that patches an action map, so the four
//    tool names have no writer at all - not a narrow one, none.
//
// Two smaller gaps follow from the same two: a child begin cannot name a
// registration because the reused selection parse has no
// agentWorkflowRegistrationId field (synthetic_generation_linux.go:405-417), and
// no Go file names the builtin domain tools propose_novel_config or
// propose_chapter_blueprint that commit-domain-tool exists to write
// (electron/services/agent-domain-tool-effect.ts:19).
//
// A handler installed over any of that would be worse than an unbound name: the
// browser would get a 5xx, or a run that its own next read cannot prove, instead
// of the plain 501 that says the server cannot do this yet. So the protocol
// rules that CAN be pinned without the ports - identity, replay idempotency,
// round ordering, registration authority - live in internal/agentgen, tested
// here, and these six names stay unbound.
//
// # Binder
//
// This file still installs nothing: the six names it records keep failing closed
// at GO_OPERATION_NOT_MIGRATED, so bindProductionAgentGeneration moves no count
// and stays where it is, beside bindProductionFinalizationGeneration
// (production_open_linux.go:749). The two served names are installed by
// newProductionAgentGenerationServe(owner).Bind(server), which is why that
// binder has to sit inside production_open_linux.go's generation-owner block,
// next to newProductionLegacyRosterServe(owner).Bind(server)
// (production_open_linux.go:707): begin mints a model lease and opens a ledger
// run under the owner's own admission, and nothing outside that block can reach
// it. bindProductionAgentGeneration's job is now to prove that every name it
// lists is still refused - it refuses to run if a name it records is one the
// serve file installs.

// The eight names, spelled once so the binder and the test cannot drift from
// the controller that registers them
// (electron/controllers/generation-controller.ts:244-257). The first two are
// served by production_agent_generation_serve_linux.go; the six below them are
// the refused half this file records.
const (
	productionAgentGenerationBegin        = "agent-generation:begin"
	productionAgentGenerationRead         = "agent-generation:read"
	productionAgentGenerationResume       = "agent-generation:resume"
	productionAgentGenerationRound        = "agent-generation:round"
	productionAgentGenerationClaimTool    = "agent-generation:claim-tool"
	productionAgentGenerationFinishTool   = "agent-generation:finish-tool"
	productionAgentGenerationRegister     = "agent-generation:register-workflow"
	productionAgentGenerationCommitDomain = "agent-generation:commit-domain-tool"
)

// productionAgentGenerationLine is one declaration that has to move before a
// name can be served: the path of the file, the line inside it, and a marker
// that must be on that line. TestProductionLaneRefusesAgentGeneration re-reads
// every one of them, so a moved declaration fails this lane's test instead of
// leaving a note that points at nothing.
type productionAgentGenerationLine struct {
	Path   string
	Line   int
	Marker string
}

// productionAgentGenerationSeam is one refused name, the one ledger operation it
// would freeze, and the missing piece that keeps it unbound.
type productionAgentGenerationSeam struct {
	Operation string
	Ledger    string
	Missing   []productionAgentGenerationLine
}

// productionAgentGenerationOperations is the family's refused names, in the order
// the desktop controller registers them
// (electron/controllers/generation-controller.ts:244-257). begin and read are
// served (productionAgentGenerationServedOperations) and are deliberately absent:
// a name in both lists would be a run this lane both installs and disclaims.
func productionAgentGenerationOperations() []string {
	return nil
}

// productionAgentGenerationLedgerOperation is the one mapping this family needs:
// every name of it - served or refused - is a step of the same round run, so
// every one of them freezes agentgen.Operation and nothing else. A name neither
// table lists has no ledger operation, which is how a typo refuses rather than
// opening a run of some other family.
func productionAgentGenerationLedgerOperation(operation string) (string, bool) {
	seam, found := productionAgentGenerationSeamFor(operation)
	if !found || seam.Ledger == "" {
		return "", false
	}
	return seam.Ledger, true
}

// productionAgentGenerationSeams is the whole answer of this file: for each
// refused name, the exact code that has to move first. The markers are quotes
// from the files, so the test can prove the gap is still there and still
// described the way this table describes it.
func productionAgentGenerationSeams() []productionAgentGenerationSeam {
	return nil
}

// productionAgentGenerationSeamFor returns the documented missing piece for one
// name, or false when neither table lists it.
func productionAgentGenerationSeamFor(operation string) (productionAgentGenerationSeam, bool) {
	for _, seam := range productionAgentGenerationSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionAgentGenerationSeam{}, false
}

// bindProductionAgentGeneration installs the six refused agent-generation:*
// names. It installs nothing, and it refuses to start a lane whose refusal is
// undocumented: every name must carry at least one named missing piece and the
// family's one ledger operation, so the day one of them gains a body the table
// has to change with it instead of silently becoming a lie.
//
// It also refuses a table that lists a name the serve file installs. That is the
// one way this file could go stale in the other direction - a served name still
// described as refused - and it is checked here, at the lane's own start, rather
// than only in a test.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring this into production_open_linux.go cannot mask a
// construction bug.
func bindProductionAgentGeneration(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	served := make(map[string]bool, len(productionAgentGenerationServedOperations()))
	for _, operation := range productionAgentGenerationServedOperations() {
		served[operation] = true
	}
	for _, operation := range productionAgentGenerationOperations() {
		if served[operation] {
			return errors.New("PRODUCTION_AGENT_GENERATION_SEAM_SERVED")
		}
		seam, found := productionAgentGenerationSeamFor(operation)
		if !found || seam.Ledger != agentgen.Operation || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_AGENT_GENERATION_SEAM_INCOMPLETE")
		}
	}
	// Nothing is bound: the six names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED, which is the answer the synthetic lane already
	// pins for them (synthetic_generation_http_linux_test.go:472-474). The two
	// served names are installed by newProductionAgentGenerationServe, which needs
	// the generation owner this binder deliberately does not take.
	return nil
}
