//go:build linux

package main

// This file serves legacy-roster:stage, the last of the seven family names. The
// reference is electron/services/legacy-roster-generation.ts:52-68: prove the
// run and its settled artifact, re-prove the frozen binding, then hand the
// canonical source to CharacterProposalService.stage's legacy arm, which writes
// one character_identity_proposals row and stamps the attempt's usage receipt
// with the legacyRosterProposal effect, all inside one immediate transaction.
//
// The transaction lives in package store (internal/store/legacy_roster_proposal.go):
// the proof and the projection are one read of the same facts, and the borrowed
// author database holds a single connection, so a package-main prover would
// deadlock against the writer's reserved connection. The store re-derives the
// candidate, the source hash and the provenance from the database, and refuses a
// manifest without a 64-hex modelReceipt.modelRevision, so a fabricated or empty
// provenance is refused by the store's own commit guard. The only thing the
// store cannot do is the in-memory binding reproduction, which this file runs
// before the write, exactly like execute does.
//
// The binder line that installs this name into the product lane sits beside
// newProductionLegacyRosterExecuteServe(owner).Bind(server) in
// production_open_linux.go's owner block (:884-889).

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionLegacyRosterArtifact is LegacyRosterGenerationArtifact
// (src/shared/legacy-roster-generation.ts): the three-key artifact reference the
// proof checks key by key.
type productionLegacyRosterArtifact struct {
	ArtifactID string
	Revision   int64
	TextHash   string
}

// productionLegacyRosterStageServe serves legacy-roster:stage over the one
// generation owner. It borrows the begin/read/cancel serve for the run proof and
// the source reproduction, so the two handlers cannot disagree about what a
// legacy-roster run is.
type productionLegacyRosterStageServe struct {
	owner  *productionGeneration
	roster *productionLegacyRosterServe
}

// newProductionLegacyRosterStageServe is the constructor production_open_linux.go
// wires beside newProductionLegacyRosterExecuteServe.
func newProductionLegacyRosterStageServe(owner *productionGeneration) *productionLegacyRosterStageServe {
	return &productionLegacyRosterStageServe{owner: owner, roster: newProductionLegacyRosterServe(owner)}
}

// ready reports whether a stage could be admitted at all.
func (s *productionLegacyRosterStageServe) ready() bool {
	return s != nil && s.owner != nil && s.owner.open != nil && s.owner.inner != nil
}

// Bind installs legacy-roster:stage. A nil owner installs nothing, so the name
// keeps failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling
// binders.
func (s *productionLegacyRosterStageServe) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if s == nil || s.owner == nil || s.owner.inner == nil {
		return nil
	}
	return server.Bind(productionLegacyRosterStageName, productionLegacyRosterStageChannel(s.stage))
}

// productionLegacyRosterStageChannel is the stage half: the channel's one
// `{ handle, artifact }` request plus the frozen three-key project context at
// args[1]. The reference refuses any request key other than handle and artifact
// with GENERATION_LEGACY_REQUEST_INVALID (main-generation-owner.ts:1139).
func productionLegacyRosterStageChannel(call func(context.Context, transport.Caller, map[string]any, syntheticHandle, productionLegacyRosterArtifact) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		expected, err := productionEditorInlineSession(args)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		handle, artifact, err := productionLegacyRosterStageRequest(args[0])
		if err != nil {
			return productionGraphReadFailure(err)
		}
		if ctx == nil {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, handle, artifact)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		return value, nil
	}
}

// productionLegacyRosterStageRequest unwraps `{ handle, artifact }` the way the
// reference does: exactly those two keys, then the four-key handle shape and the
// three-key artifact shape.
func productionLegacyRosterStageRequest(value any) (syntheticHandle, productionLegacyRosterArtifact, error) {
	request := syntheticMap(value)
	if request == nil {
		return syntheticHandle{}, productionLegacyRosterArtifact{}, errors.New(productionLegacyRosterRequestInvalid)
	}
	for key := range request {
		switch key {
		case "handle", "artifact":
		default:
			return syntheticHandle{}, productionLegacyRosterArtifact{}, errors.New(productionLegacyRosterRequestInvalid)
		}
	}
	handle, err := productionLegacyRosterHandleRequest(map[string]any{"handle": request["handle"]})
	if err != nil {
		return syntheticHandle{}, productionLegacyRosterArtifact{}, err
	}
	artifact, err := productionLegacyRosterArtifactRequest(request["artifact"])
	if err != nil {
		return syntheticHandle{}, productionLegacyRosterArtifact{}, err
	}
	return handle, artifact, nil
}

// productionLegacyRosterArtifactRequest unwraps the three-key artifact reference
// (legacy-roster-generation-proof.ts:20).
func productionLegacyRosterArtifactRequest(value any) (productionLegacyRosterArtifact, error) {
	invalid := errors.New(productionLegacyRosterArtifactInvalid)
	artifact := syntheticMap(value)
	if artifact == nil {
		return productionLegacyRosterArtifact{}, invalid
	}
	for key := range artifact {
		switch key {
		case "artifactId", "revision", "textHash":
		default:
			return productionLegacyRosterArtifact{}, invalid
		}
	}
	artifactID := syntheticString(artifact["artifactId"])
	revision, revisionOK := syntheticFloat64(artifact["revision"])
	textHash := syntheticString(artifact["textHash"])
	if artifactID == "" || !revisionOK || revision < 0 || revision > productionLegacyRosterMaxSafeInteger ||
		math.Trunc(revision) != revision || textHash == "" {
		return productionLegacyRosterArtifact{}, invalid
	}
	return productionLegacyRosterArtifact{ArtifactID: artifactID, Revision: int64(revision), TextHash: textHash}, nil
}

// stage answers legacy-roster:stage. It resolves the run, reproduces the frozen
// binding, then hands the proven handle and artifact to the store's one
// transaction, which re-proves the artifact and candidate, reads the frozen model
// revision and writes the proposal plus the effect.
func (s *productionLegacyRosterStageServe) stage(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle, artifact productionLegacyRosterArtifact) (any, error) {
	if !s.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := s.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := s.roster.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	// assertSources: the frozen binding must still reproduce before a proposal is
	// staged from a run whose sources moved (legacy-roster-generation.ts:61).
	runScope := scope
	if bindingMap := syntheticMap(resolved.Run["binding"]); bindingMap != nil {
		runScope.Project = syntheticString(bindingMap["projectId"])
		runScope.Epoch = syntheticString(bindingMap["epoch"])
	}
	_, fingerprint, err := s.roster.reproduce(ctx, runScope, db, resolved)
	if err != nil {
		return nil, err
	}
	if fingerprint != syntheticString(resolved.Manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New(productionLegacyRosterSourceChanged)
	}
	return store.NewLegacyRosterProposals(db).Stage(ctx, store.LegacyRosterStageRequest{
		ProjectID:    scope.Project,
		Epoch:        handle.Epoch,
		RootActionID: handle.RootActionID,
		RunID:        handle.RunID,
		ArtifactID:   artifact.ArtifactID,
		Revision:     artifact.Revision,
		TextHash:     artifact.TextHash,
	})
}
