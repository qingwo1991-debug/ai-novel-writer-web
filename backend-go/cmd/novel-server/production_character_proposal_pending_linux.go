//go:build linux

package main

// This file serves the last two `character-proposal` names, the ones that page
// batches produced by a finalized-character generation or by an import:
//
//	character-proposal:list-pending-finalized  web/operations.json:58  served here
//	character-proposal:read-pending-finalized  web/operations.json:59  served here
//
// # The reference
//
//	electron/controllers/generation-controller.ts:213-214
//	  register('character-proposal:list-pending-finalized', 0, owner => owner.characterProposals.listPendingFinalized())
//	  register('character-proposal:read-pending-finalized', 1, (owner, request) => owner.characterProposals.readPendingFinalized(request.proposalBatchId))
//	electron/services/character-proposal-service.ts
//	  :131-147 listPendingFinalized  :149-154 readPendingFinalized
//	  :60-102  readEnvelope         :103-112 resolveItems
//	electron/services/finalized-character-generation-proof.ts:13-64
//	  proveFinalizedCharacterGeneration, the `finalized-generation` proof
//	electron/services/generation-character-proposal-proof.ts:67-103
//	  the `import` proof
//
// The wire declarations are src/shared/character-proposal.ts:77-78: the list
// takes no request at all and answers PendingFinalizedCharacterProposalSummary[];
// the read takes `{ proposalBatchId }` and answers CharacterProposalBatch. The
// only consumer is src/components/characters/use-pending-character-proposals.ts:30
// and :47, which pages the summaries and then reads one by its batch id.
//
// # The store half
//
// The proof and the projection are one read of the same two facts, so they run
// in one BEGIN IMMEDIATE on one reserved connection inside package store
// (internal/store/character_proposal_pending.go). That is the composition the
// family's earlier record said was impossible; it is not - package store has
// opened that handle over these very rows since summary.go:681 and
// finalized_draft_import.go:900. A package-main prover could not be used here in
// any case: the borrowed author database holds one connection
// (internal/projectfs/author_linux.go:121), so a second transaction on it while
// the immediate one runs deadlocks.
//
// The proof's own comparison is reproducible without a TypeScript encoder. The
// reference compares the stored proof with a re-proved one by value
// (isDeepStrictEqual), and stores one SHA-256 over JSON.stringify. The store
// parses the row's own JSON with its order-preserving decoder and re-encodes it
// with the package's JSON.stringify-compatible serializer, so the hash is taken
// over the stored document's own key order - the desktop's bytes - rather than
// over a typed Go struct's. `byte-exactness` was never the obstacle it was
// recorded as.
//
// # The generated arm inside the served answer
//
// The generated arm of the import proof decodes the settled import-inference
// artifact (decodeImportInferenceJson, import-inference-contract.ts:257) and
// compares the decoded cards with the receipt entries
// (generation-character-proposal-proof.ts:97-98). That decoder is now ported
// (internal/store/import_inference_decode.go), together with the two bounded
// corrections it composes, so an import proposal whose import_effect receipt
// carries a generationRunHandle is proved exactly as the reference proves it:
// the run's artifact either decodes to the receipt's own entries, which is a
// `generated` provenance naming the run's modelRevision, or it does not, which
// is the reference's null provenance rather than a guessed one. A malformed
// inference is never refused by name; the reference catches every contract
// refusal into the null arm, and so does the port.
//
// # The wiring
//
// The binder line belongs beside the family's own binder in
// production_open_linux.go, which this batch may not edit:
//
//	func() error { return newProductionCharacterProposalPending(open.durable).Bind(server) },
//
// It installs exactly these two names and nothing else.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionCharacterProposalPending is the borrowed lane for the two
// pending-finalized names. It holds no database: every handler borrows the
// project the frozen context names, so it can never read a database the
// admission did not prove.
type productionCharacterProposalPending struct {
	durable *productionDurableOpen
}

// newProductionCharacterProposalPending is the constructor the binder line
// above installs.
func newProductionCharacterProposalPending(durable *productionDurableOpen) *productionCharacterProposalPending {
	return &productionCharacterProposalPending{durable: durable}
}

// Bind installs `character-proposal:list-pending-finalized` and
// `character-proposal:read-pending-finalized` and nothing else. A nil or closed
// bridge installs nothing, so both names keep failing closed at
// GO_OPERATION_NOT_MIGRATED instead of being published as handlers that can only
// refuse.
func (p *productionCharacterProposalPending) Bind(server *transport.Server) error {
	if p == nil || p.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	bindings := []struct {
		Name  string
		Arity int
		Serve func(context.Context, *sql.DB, productionCharacterProposalScope, any) (any, error)
	}{
		{Name: productionCharacterProposalListPendingName, Arity: 1,
			Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, _ any) (any, error) {
				return productionCharacterProposalPendingList(ctx, db, scope)
			}},
		{Name: productionCharacterProposalReadPendingName, Arity: 2,
			Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
				return productionCharacterProposalPendingRead(ctx, db, scope, body)
			}},
	}
	for _, binding := range bindings {
		serve := binding.Serve
		arity := binding.Arity
		if err := server.Bind(binding.Name, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			// Wire arity is the request arity - zero for the list, one for the
			// read - plus the frozen context.
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			admitted, err := projectsession.ParseContext(args[arity-1])
			if err != nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			if ctx == nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			db, release, err := p.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
			if err != nil {
				// Already symbolic: a lease, ownership or closed-lane refusal.
				return nil, err
			}
			if db == nil || release == nil {
				return nil, errors.New(productionCharacterProposalRequestFailed)
			}
			defer release()
			var body any
			if arity > 1 {
				body = args[0]
			}
			result, err := serve(ctx, db, productionCharacterProposalScope{ProjectID: admitted.ProjectID, Epoch: admitted.LeaseID}, body)
			if err != nil {
				return nil, productionCharacterProposalFailure(err)
			}
			return result, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// productionCharacterProposalPendingList is listPendingFinalized (:131-147): the
// summaries of the pending proposals whose source still proves and whose
// provenance carries a source. An import summary carries the operation, the item
// count and, when the run row still exists, its purpose (:125-130); a
// finalized-generation summary carries the finalization the provenance names.
func productionCharacterProposalPendingList(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope) (any, error) {
	proposals, err := store.NewPendingFinalized(db).List(ctx, scope.ProjectID)
	if err != nil {
		return nil, err
	}
	summaries := make([]any, 0, len(proposals))
	for _, proposal := range proposals {
		summary := map[string]any{
			"proposalBatchId": proposal.ProposalBatchID,
			"revision":        proposal.Revision,
		}
		if proposal.Kind == productionCharacterProposalFinalizedKind {
			summary["finalizationId"] = proposal.FinalizationID
		} else {
			summary["importOperationId"] = proposal.OperationID
			summary["itemCount"] = proposal.ItemCount
			if proposal.ImportPurpose != "" {
				summary["importPurpose"] = proposal.ImportPurpose
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// productionCharacterProposalPendingRead is readPendingFinalized (:149-154). The
// store has already re-proved the source, asserted the finalization is still
// current, and refreshed the resolution against the live roster in one
// transaction; the answer is the batch that transaction left in the row.
func productionCharacterProposalPendingRead(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
	request, ok := body.(map[string]any)
	if !ok {
		return nil, productionCharacterProposalReceiptInvalid
	}
	batchID, ok := request[productionCharacterProposalBatchKey].(string)
	if !ok || strings.TrimSpace(batchID) == "" {
		// The reference binds the id into its own SELECT, so an absent id is a
		// row that cannot be found, which is the receipt refusal.
		return nil, productionCharacterProposalReceiptInvalid
	}
	proposal, err := store.NewPendingFinalized(db).Read(ctx, scope.ProjectID, batchID)
	if err != nil {
		return nil, err
	}
	var envelope map[string]any
	decoder := json.NewDecoder(strings.NewReader(proposal.Envelope))
	// The batch is re-encoded on the wire; UseNumber keeps a revision or an
	// evidence offset the integer the reference wrote.
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	batch, ok := envelope["batch"]
	if !ok || batch == nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	return batch, nil
}
