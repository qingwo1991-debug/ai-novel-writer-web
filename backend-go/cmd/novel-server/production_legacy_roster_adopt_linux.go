//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves `legacy-roster:adopt-existing`, the one write in the
// legacy-roster family. The reference (electron/services/legacy-roster-source.ts:71-104)
// re-reads the source, re-proves it against four CAS tokens, rebuilds the
// read-only projection from the cards the author already has, and records the
// author's approval receipt - all in ONE transaction, so a failure between the
// projection and the receipt rolls both back together.
//
// That one transaction now lives in the store (internal/store/character_roster_adoption.go):
// one reserved connection, one BEGIN IMMEDIATE, the projection refresh
// (characterIdentityRefreshProjection) and the approval INSERT on the connection
// it was given. The only fact the store cannot compute is the raw-row factsHash -
// the JS-faithful sha256 over the three SELECT * row sets - which is this file's
// productionLegacyRosterJSON. So the handler passes a `source` proof that runs on
// the store's reserved connection, and everything else stays in the store.
//
// The wire arity is the request arity (one, generation-controller.ts:234) plus
// the frozen context, so exactly two arguments arrive: the AdoptLegacyCardsRequest
// and the project context. The borrow and the identity-schema guard run before the
// store transaction, exactly like the reference's own :76.

// adoptExisting is the handler productionLegacyRoster.Bind installs for
// `legacy-roster:adopt-existing`.
func (l *productionLegacyRoster) adoptExisting(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 2 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	if _, err := projectsession.ParseContext(args[1]); err != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	if ctx == nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	db, release, err := l.durable.borrowAuthorDatabase(ctx, caller, args[1])
	if err != nil {
		// Already symbolic: a lease, ownership or closed-lane refusal.
		return nil, err
	}
	if db == nil || release == nil {
		return nil, errors.New(productionLegacyRosterRequestFailed)
	}
	defer release()
	// :76 hasCharacterIdentitySchema is the reference's first adoption refusal.
	present, err := productionLegacyRosterIdentitySchema(ctx, db)
	if err != nil {
		return nil, productionLegacyRosterFailure(err)
	}
	if !present {
		return nil, productionLegacyRosterFailure(productionLegacyRosterIDSchemaRequired)
	}
	answer, err := store.NewCharacters(db).AdoptLegacyRoster(ctx, syntheticMap(args[0]), productionLegacyRosterAdoptSource)
	if err != nil {
		return nil, productionLegacyRosterFailure(err)
	}
	return answer, nil
}

// productionLegacyRosterAdoptSource is the source proof the adoption runs inside
// the store's transaction: the same readLegacyRosterSource guards and the same
// raw-row facts hash, over the caller's reserved connection. The roster snapshot
// is read by the store afterwards, so this half never re-reads it or opens a
// second pool connection.
func productionLegacyRosterAdoptSource(ctx context.Context, conn *sql.Conn) (store.LegacyRosterAdoptionSource, error) {
	facts, err := productionLegacyRosterSourceFactsOn(ctx, conn)
	if err != nil {
		return store.LegacyRosterAdoptionSource{}, err
	}
	return store.LegacyRosterAdoptionSource{
		RawLegacy:        facts.RawLegacy,
		LegacyHash:       facts.LegacyHash,
		IdentityRevision: facts.IdentityRevision,
		ActiveIDs:        facts.ActiveIDs,
		FactsHash:        facts.FactsHash,
	}, nil
}
