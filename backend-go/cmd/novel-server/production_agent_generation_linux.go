//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is what is left of the agent-generation:* refusal lane, and it no
// longer refuses anything. All eight names of the family are served:
//
//	agent-generation:begin               production_agent_generation_serve_linux.go
//	agent-generation:read                production_agent_generation_serve_linux.go
//	agent-generation:resume              production_agent_generation_tools_linux.go
//	agent-generation:round               production_agent_generation_tools_linux.go
//	agent-generation:claim-tool          production_agent_generation_tools_linux.go
//	agent-generation:finish-tool         production_agent_generation_tools_linux.go
//	agent-generation:register-workflow   production_agent_generation_tools_linux.go
//	agent-generation:commit-domain-tool  production_agent_generation_tools_linux.go
//
// so productionAgentGenerationOperations and productionAgentGenerationSeams are
// empty by design and bindProductionAgentGeneration installs nothing. What stays
// here is the family's one ledger mapping and the shared name constants; the two
// tables and the binder remain because several tests read them as the declaration
// that the refusal half is empty, not because a name still fails closed.
//
// The family is one ledger operation, not eight: a round is a generation run.
// The desktop owner freezes operation 'agent-round' for every round it opens
// (electron/services/agent-generation.ts:175-178), the ledger admits the
// agentSession manifest key for exactly that operation
// (internal/store/generation_ledger.go:428-429), and every identity check in the
// reference refuses a run whose operation is anything else
// (electron/services/agent-generation.ts:54-62). The ledger was never the gap; it
// is what productionAgentGenerationLedgerOperation names once.
//
// # The two ports the six tool names needed, and got
//
// 1. The tool-call protocol on the dispatch path. A round is a model call whose
//    reply is parsed into visible text plus tool calls
//    (electron/services/generation-run-service.ts:263-266, over
//    src/shared/agent-response-protocol.ts:136), and the parse only runs because
//    the request carries agentToolNames. internal/generation now admits that
//    field for exactly one operation - agent-round
//    (internal/generation/service.go:277, over agentRoundOperation at :235) -
//    and re-attaches the parsed response as result.usage.agentResponse when a
//    completed stop settles (:533-539). Every other family keeps the refusal, so
//    opening the field never puts the agent protocol behind an ordinary run.
//
// 2. The per-action receipt state. Every tool operation of the family is a write
//    to the attempt's usage receipt: claim stamps agentActions
//    (electron/services/agent-generation.ts:215-232), finish stamps the settled
//    action (:259-275), register stamps the workflow metadata (:276-293),
//    commit-domain-tool stamps the domain effect plus the source transition
//    (:377-404), and resume stamps the control epoch (:163-170). The ledger
//    writes them through recordAgentReceipt
//    (internal/store/generation_ledger_agent.go:76), whose allowlist admits
//    exactly agentActions, agentAuthorSelections, agentSourceTransition and
//    agentControlEpoch, gated to operation agent-round. It is deliberately not
//    recordEffect: the action map is keyed by tool call id and merged by the
//    caller, so the ledger proves the operation owns the keys and stores them in
//    one effect transaction.
//
// The builtin domain tools propose_novel_config and propose_chapter_blueprint
// that commit-domain-tool writes are named in
// production_agent_generation_tools_linux.go. The child registration key
// agentWorkflowRegistrationId is still not minted by this family's begin: it
// belongs to the child-effect stamp internal/store/projectcore_generated_tx.go
// refuses as GENERATION_AGENT_CHILD_EFFECT_UNPORTED.
//
// # Binder
//
// This file still installs nothing, but not because anything is left to refuse.
// bindProductionAgentGeneration is still called (production_open_linux.go:954) so
// that a name added back to the refusal table can never be installed by the serve
// binder without the two tables agreeing, and it keeps the nil-server refusal
// every binder in this package answers. The eight served names are installed by
// newProductionAgentGenerationServe(owner).Bind(server), which sits inside
// production_open_linux.go's generation-owner block
// (production_open_linux.go:892): begin mints a model lease and opens a ledger
// run under the owner's own admission, and nothing outside that block can reach
// it.

// The eight names, spelled once so the binder and the test cannot drift from
// the controller that registers them
// (electron/controllers/generation-controller.ts:244-257). The first two are
// served by production_agent_generation_serve_linux.go and the six below them by
// production_agent_generation_tools_linux.go; nothing in this file installs any
// of them.
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

// productionAgentGenerationLine is one declaration that had to move before a
// refused name could be served: the path of the file, the line inside it, and a
// marker that must be on that line. TestProductionLaneRefusesAgentGeneration
// re-reads every row of the seam table, so a seam that is still documented after
// its declaration moves fails this lane's test instead of leaving a note that
// points at nothing. With the table empty it reads nothing, which is the
// declaration that the family is fully served.
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

// productionAgentGenerationOperations is the family's refused names. It is empty:
// all eight names are served (productionAgentGenerationServedOperations), so no
// name is left for this table to disclaim, and a name in both lists would be a
// run the lane both installs and refuses. The controller order it once mirrored
// is electron/controllers/generation-controller.ts:244-257.
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

// productionAgentGenerationSeams is empty for the same reason
// productionAgentGenerationOperations is: a seam row exists to name the code that
// has to move before a refused name can be served, and every name of the family
// is already served. The machinery stays so the tables, the ledger mapping and
// the binder keep one shape.
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

// bindProductionAgentGeneration installs none of the agent-generation:* names,
// because none of them is refused. It is kept as the refusal lane's own guard: a
// name that comes back to the refusal table must carry at least one named missing
// piece and the family's one ledger operation, and must not also be a name the
// serve file installs. Checking that here, at the lane's own start rather than
// only in a test, is what keeps the two tables from drifting apart in either
// direction.
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
	// Nothing is bound: there is nothing left to refuse. The eight served names
	// are installed by newProductionAgentGenerationServe, which needs the
	// generation owner this binder deliberately does not take. The synthetic
	// durable lane binds no agent-generation name at all, so its own 501 inventory
	// (synthetic_generation_http_linux_test.go:472-474) still holds.
	return nil
}
