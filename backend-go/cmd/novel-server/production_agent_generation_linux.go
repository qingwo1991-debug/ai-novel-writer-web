//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is where the eight agent-generation:* operations would be served,
// and it serves none of them. Each stays at the fail-closed 501
// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116), and each carries
// the exact code that has to move before a handler can be installed instead of
// a note that points at nothing.
//
// The family is one ledger operation, not eight: a round is a generation run.
// The desktop owner freezes operation 'agent-round' for every round it opens
// (electron/services/agent-generation.ts:175-178), the ledger admits the
// agentSession manifest key for exactly that operation
// (internal/store/generation_ledger.go:428-429), and every identity check in the
// reference refuses a run whose operation is anything else
// (electron/services/agent-generation.ts:54-62). So the ledger is NOT what is
// missing here, and this file's mapping layer says so once:
// productionAgentGenerationLedgerOperation. What is missing is the round
// protocol itself, and it is missing in three places.
//
// # The missing ports
//
// 1. The agent context begin freezes. buildAgentGenerationContext
//    (electron/services/agent-generation-context.ts:32) resolves the
//    `assistant_writing_identity` PromptTemplate, composes its system role and
//    appends the builtin suffix, the project-core JSON and the tool manifest
//    into initialMessages, then freezes the whole value as
//    sourceManifest.agentContext with its own hash
//    (electron/services/generation-source-binding.ts:593). No Go file in this
//    module names PromptTemplate or renderPrompt - the same gap
//    production_finalization_generation_linux.go:51 already records - and the
//    lane's prompt source hands back a placeholder instead of a template
//    (synthetic_generation_linux.go:645). A begin here could freeze a session
//    key and a round index, but not the messages the first round would send, so
//    it would open a run whose very next call cannot use it.
//
// 2. The tool-call protocol on the dispatch path. A round is a model call whose
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
// 3. The per-action receipt state. Every tool operation of the family is a
//    write to the attempt's usage receipt: claim stamps agentActions
//    (electron/services/agent-generation.ts:215-232), finish stamps the settled
//    action (:259-275), register stamps the workflow metadata (:276-293),
//    commit-domain-tool stamps the domain effect plus the source transition
//    (:377-404), and resume stamps the control epoch (:163-170). The ledger's
//    closed receipt allowlist admits none of those top-level keys
//    (internal/store/generation_ledger.go:588, refused as
//    GENERATION_EFFECT_UNSUPPORTED) and the only method that patches a receipt
//    key outside settle is recordEffect, whose own allowlist is effect-only
//    (:1215-1226). There is no ledger method that patches an action map, so the
//    four tool names have no writer at all - not a narrow one, none.
//
// Two smaller gaps follow from the same three: a child begin cannot name a
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
// here, and nothing is bound.
//
// # Binder
//
// bindProductionAgentGeneration installs nothing, so wiring it moves no count:
// the served set stays at productionServedOperations
// (production_served_count_linux_test.go:22). It is wired the way the
// finalization slice is - one entry in the binder list in
// production_open_linux.go, next to bindProductionFinalizationGeneration
// (production_open_linux.go:674) - and what that buys is the seam check running
// in the real lane instead of only in this test. When a port above lands, the
// handler is installed with productionGenerationMutation
// (production_generation_write_linux.go:174) and an arity of 1, exactly like
// graph-generation:begin (production_open_linux.go:567).

// The eight names, spelled once so the binder and the test cannot drift from
// the controller that registers them
// (electron/controllers/generation-controller.ts:244-257).
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

// productionAgentGenerationOperations is the family's eight names, in the order
// the desktop controller registers them
// (electron/controllers/generation-controller.ts:244-257).
func productionAgentGenerationOperations() []string {
	return []string{
		productionAgentGenerationBegin,
		productionAgentGenerationRead,
		productionAgentGenerationResume,
		productionAgentGenerationRound,
		productionAgentGenerationClaimTool,
		productionAgentGenerationFinishTool,
		productionAgentGenerationRegister,
		productionAgentGenerationCommitDomain,
	}
}

// productionAgentGenerationLedgerOperation is the one mapping this family needs:
// every one of the eight names is a step of the same round run, so every one of
// them freezes agentgen.Operation and nothing else. A name the table does not
// list has no ledger operation, which is how a typo refuses rather than opening
// a run of some other family.
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
	return []productionAgentGenerationSeam{
		{
			Operation: productionAgentGenerationBegin,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// The frozen context is assembled from the identity template, and
				// this lane resolves no template: its prompt source is a placeholder
				// string built from the key.
				{Path: "synthetic_generation_linux.go", Line: 645, Marker: `"synthetic:" + key.ID`},
				// The same missing prompt-template port, already recorded by the
				// finalization slice for the same reason.
				{Path: "production_finalization_generation_linux.go", Line: 51, Marker: "renderPrompt"},
				// The port itself, on the other side of the boundary.
				{Path: "../../../electron/services/agent-generation-context.ts", Line: 32, Marker: "buildAgentGenerationContext"},
			},
		},
		{
			Operation: productionAgentGenerationRead,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// A recovery is projected from the parsed response, which no attempt
				// can carry while the dispatch service refuses agent tool manifests.
				{Path: "../../internal/generation/service.go", Line: 261, Marker: "if r.AgentToolNames != nil"},
				{Path: "../../../src/shared/agent-response-protocol.ts", Line: 136, Marker: "parseAgentResponseProtocol"},
				// And the action map a projection reports cannot be stored at all: the
				// closed receipt allowlist refuses every key that would carry it.
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
			},
		},
		{
			Operation: productionAgentGenerationResume,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// Resume re-activates the root and stamps the control epoch onto the
				// attempt receipt, which is the same receipt port as below.
				{Path: "../../../electron/services/agent-generation.ts", Line: 156, Marker: "async resume(handle"},
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
				{Path: "../../internal/generation/service.go", Line: 261, Marker: "if r.AgentToolNames != nil"},
			},
		},
		{
			Operation: productionAgentGenerationRound,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// The protocol parse and the stamp it produces are the round's whole
				// result, and the dispatch service refuses the manifest that enables it.
				{Path: "../../internal/generation/service.go", Line: 261, Marker: "if r.AgentToolNames != nil"},
				{Path: "../../../electron/services/generation-run-service.ts", Line: 266, Marker: "agentResponse: { version: 1"},
				{Path: "../../../src/shared/agent-response-protocol.ts", Line: 136, Marker: "parseAgentResponseProtocol"},
			},
		},
		{
			Operation: productionAgentGenerationClaimTool,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// The claim writes agentActions[actionId].status, and no ledger method
				// patches an action map: recordEffect is the only receipt patch port
				// and its allowlist is effect-only.
				{Path: "../../../electron/services/agent-generation.ts", Line: 230, Marker: "claimTool(request"},
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
				{Path: "../../internal/store/generation_ledger.go", Line: 1215, Marker: `case "recordEffect":`},
			},
		},
		{
			Operation: productionAgentGenerationFinishTool,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				{Path: "../../../electron/services/agent-generation.ts", Line: 259, Marker: "finishTool(request"},
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
				// A completed builtin domain tool must carry a domain write, and the
				// domain writer is the commit-domain-tool gap below.
				{Path: "../../../electron/services/agent-domain-tool-effect.ts", Line: 19, Marker: "export function commitAgentDomainTool"},
			},
		},
		{
			Operation: productionAgentGenerationRegister,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// The registration is stamped into the same action map the claim writes.
				{Path: "../../../electron/services/agent-generation.ts", Line: 276, Marker: "registerWorkflow(ref"},
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
				// And the metadata is spent by a child begin that names it, which the
				// reused selection parse cannot do: it has no registration field.
				{Path: "synthetic_generation_linux.go", Line: 405, Marker: "func syntheticParseSelection("},
			},
		},
		{
			Operation: productionAgentGenerationCommitDomain,
			Ledger:    agentgen.Operation,
			Missing: []productionAgentGenerationLine{
				// The domain effect writers do not exist in Go: no file in this module
				// names propose_novel_config or propose_chapter_blueprint.
				{Path: "../../../electron/services/agent-generation.ts", Line: 377, Marker: "commitDomainTool(ref"},
				{Path: "../../../electron/services/agent-domain-tool-effect.ts", Line: 19, Marker: "export function commitAgentDomainTool"},
				{Path: "../../internal/store/generation_ledger.go", Line: 588, Marker: `"artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition"`},
			},
		},
	}
}

// productionAgentGenerationSeamFor returns the documented missing piece for one
// name, or false when the name is not one of the eight.
func productionAgentGenerationSeamFor(operation string) (productionAgentGenerationSeam, bool) {
	for _, seam := range productionAgentGenerationSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionAgentGenerationSeam{}, false
}

// bindProductionAgentGeneration installs the eight agent-generation:* names. It
// installs nothing, and it refuses to start a lane whose refusal is
// undocumented: every name must carry at least one named missing piece and the
// family's one ledger operation, so the day one of them gains a body the table
// has to change with it instead of silently becoming a lie.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring this into production_open_linux.go cannot mask a
// construction bug.
func bindProductionAgentGeneration(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionAgentGenerationOperations() {
		seam, found := productionAgentGenerationSeamFor(operation)
		if !found || seam.Ledger != agentgen.Operation || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_AGENT_GENERATION_SEAM_INCOMPLETE")
		}
	}
	// Nothing is bound: the eight names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED, which is the answer the synthetic lane already
	// pins for them (synthetic_generation_http_linux_test.go:472-474).
	return nil
}
