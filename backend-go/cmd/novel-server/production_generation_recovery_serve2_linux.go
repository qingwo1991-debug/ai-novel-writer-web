//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file records the answer that licensed serving the last two recovery names,
// and the wiring that serves them. The null/empty question was settled by the
// previous pass and still holds:
//
//   - list-recovery answers five required arrays plus a boolean, never null
//     (main-generation-owner.ts:1015, :1049; contract :254-262);
//   - find-blueprint-recovery is nullable and answers null when no run qualifies
//     (main-generation-owner.ts:1065, :1072).
//
// What changed this pass is the wiring: the family's binder now receives the
// durable open for the batches bucket and find-blueprint-recovery, and the owner
// for the editors bucket is recorded by the owner-block call site. The served
// count moves 279 -> 281.

// The answers this file publishes.
const (
	// productionRecoveryServe2EmptyNotNull is list-recovery: an empty bucket is an
	// empty array, never null and never a thrown call.
	productionRecoveryServe2EmptyNotNull = "EMPTY_NOT_NULL"
	// productionRecoveryServe2NullOnAbsent is find-blueprint-recovery: the name is
	// nullable and answers null when no run qualifies.
	productionRecoveryServe2NullOnAbsent = "NULL_ON_ABSENT"
	// productionRecoveryServe2StateMembership is the membership rule both names
	// share: a run is listed or returned by its continuation state.
	productionRecoveryServe2StateMembership = "MEMBERSHIP_STATE_DRIVEN"
	// productionRecoveryServe2OptionalProjection is the context's own shape: the
	// planning continuation is an optional FIELD of the context.
	productionRecoveryServe2OptionalProjection = "OPTIONAL_FIELD"
	// productionRecoveryServe2DurableBinder is the call site: the binder receives
	// the durable open (and the owner is recorded inside the owner block).
	productionRecoveryServe2DurableBinder = "BINDER_HOLDS_DURABLE"
	// productionRecoveryServe2CountChanged is the consequence: two names served.
	productionRecoveryServe2CountChanged = "LANE_COUNT_CHANGED"
)

// The claim names, one per statement this file makes.
const (
	productionRecoveryServe2ClaimListBuckets = "list-recovery buckets"
	productionRecoveryServe2ClaimFindNull    = "find-blueprint-recovery null"
	productionRecoveryServe2ClaimMembership  = "recovery membership"
	productionRecoveryServe2ClaimContext     = "GenerationRecoveryContext shape"
	productionRecoveryServe2ClaimCallSite    = "recovery binder call site"
	productionRecoveryServe2ClaimCount       = "served operation count"
)

// productionRecoveryServe2Claim is one statement, the answer this file publishes
// for it, and the declarations that decide it. Every row is re-read from disk by
// the test with productionSeamContains.
type productionRecoveryServe2Claim struct {
	Name     string
	Answer   string
	Evidence []productionGenerationRecoveryLine
}

// productionRecoveryServe2Claims is the whole answer of this pass.
func productionRecoveryServe2Claims() []productionRecoveryServe2Claim {
	return []productionRecoveryServe2Claim{
		{
			Name:   productionRecoveryServe2ClaimListBuckets,
			Answer: productionRecoveryServe2EmptyNotNull,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1015,
					Marker: "const list: GenerationRecoveryList = { planning: [], editors: [], reviews: [], batches: [], drafts: [], partial: false }"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1049,
					Marker: "if (!batchRoots.length) return list"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 254,
					Marker: "export interface GenerationRecoveryList {"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 261,
					Marker: "partial: boolean"},
			},
		},
		{
			Name:   productionRecoveryServe2ClaimFindNull,
			Answer: productionRecoveryServe2NullOnAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1065,
					Marker: "const findBlueprintRecovery = (): GenerationRecoveryContext | null => {"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1070,
					Marker: "if (recovery.blueprintRecovery && recovery.blueprintRecovery.writeState !== 'complete' && recovery.planningContinuation?.state !== 'complete') return recovery } return null"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 937,
					Marker: "editRange: null, saved: null, writeState: 'unavailable' as const, diagnostic: blueprint.diagnostic,"},
			},
		},
		{
			Name:   productionRecoveryServe2ClaimMembership,
			Answer: productionRecoveryServe2StateMembership,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1023,
					Marker: "if ((recovery.plotOutlineRecovery || recovery.blueprintRecovery) && recovery.planningContinuation?.state !== 'complete'"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 810,
					Marker: "if (!remainingRange) return { ...base, state: 'complete' }"},
			},
		},
		{
			Name:   productionRecoveryServe2ClaimContext,
			Answer: productionRecoveryServe2OptionalProjection,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 139,
					Marker: "export interface GenerationRecoveryContext {"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 142,
					Marker: "planningContinuation?: PlanningContinuation"},
			},
		},
		{
			Name:   productionRecoveryServe2ClaimCallSite,
			Answer: productionRecoveryServe2DurableBinder,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 100,
					Marker: "func bindProductionGenerationRecoveryServed(durable *productionDurableOpen, server *transport.Server) error"},
				{Path: "production_generation_recovery_serve3_linux.go", Line: 32,
					Marker: "func bindProductionGenerationRecoveryOwner(owner *productionGeneration, server *transport.Server) error"},
				{Path: "../../internal/store/batch_progress.go", Line: 207,
					Marker: "func ListGenerationBatchHistory(ctx context.Context, db *sql.DB, projectID string) ([]GenerationBatchHistory, error) {"},
			},
		},
		{
			Name:   productionRecoveryServe2ClaimCount,
			Answer: productionRecoveryServe2CountChanged,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 93,
					Marker: "server.Bind(productionGenerationRecoveryList, serve.list())"},
				{Path: "production_generation_recovery_serve3_linux.go", Line: 96,
					Marker: "server.Bind(productionGenerationRecoveryFind, serve.find())"},
			},
		},
	}
}

// productionRecoveryServe2AnswerKnown reports whether one answer is one this file
// publishes. A typo therefore fails startup instead of being recorded as a new
// kind of finding.
func productionRecoveryServe2AnswerKnown(answer string) bool {
	switch answer {
	case productionRecoveryServe2EmptyNotNull, productionRecoveryServe2NullOnAbsent,
		productionRecoveryServe2StateMembership, productionRecoveryServe2OptionalProjection,
		productionRecoveryServe2DurableBinder, productionRecoveryServe2CountChanged:
		return true
	}
	return false
}

// productionRecoveryServe2SettledClaims is the two claims that answer the question
// a later pass must not have to ask again.
func productionRecoveryServe2SettledClaims() []string {
	return []string{
		productionRecoveryServe2ClaimListBuckets,
		productionRecoveryServe2ClaimFindNull,
	}
}

// productionRecoveryServe2ClaimFor looks one claim up by name.
func productionRecoveryServe2ClaimFor(name string) (productionRecoveryServe2Claim, bool) {
	for _, claim := range productionRecoveryServe2Claims() {
		if claim.Name == name {
			return claim, true
		}
	}
	return productionRecoveryServe2Claim{}, false
}

// bindProductionGenerationRecoveryServe2 installs nothing and makes this file's
// answer a startup fact: the two names must be bound in the finished lane, or the
// claim that this pass served them is stale.
func bindProductionGenerationRecoveryServe2(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	claims := productionRecoveryServe2Claims()
	if len(claims) == 0 {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_CLAIMS_MISSING")
	}
	named := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if claim.Name == "" || named[claim.Name] || len(claim.Evidence) == 0 ||
			!productionRecoveryServe2AnswerKnown(claim.Answer) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_CLAIM_INVALID")
		}
		named[claim.Name] = true
	}
	for _, name := range productionRecoveryServe2SettledClaims() {
		if !named[name] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_ANSWER_MISSING")
		}
	}
	bound := server.BoundOperations()
	for _, operation := range productionGenerationRecoveryServedOperations() {
		if operation == productionGenerationRecoveryDirectory {
			continue
		}
		if !productionRecoveryBoundContains(bound, operation) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_NAME_MISSING")
		}
	}
	return nil
}
