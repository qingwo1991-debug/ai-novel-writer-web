//go:build linux

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file pins the answer that licensed serving the two names, and the wiring
// that serves them: the claims table, the wiring file's two call sites, and the
// answer binder's verification against a lane that really binds both names.

// TestProductionGenerationRecoveryServe2Answers re-reads every claim and pins the
// six answers, including the two that changed this pass: the binder now holds the
// durable open and the served count moved.
func TestProductionGenerationRecoveryServe2Answers(t *testing.T) {
	claims := productionRecoveryServe2Claims()
	if len(claims) == 0 {
		t.Fatal("the answer table is empty")
	}
	named := make(map[string]productionRecoveryServe2Claim, len(claims))
	for _, claim := range claims {
		if claim.Name == "" || named[claim.Name].Name != "" {
			t.Fatalf("the answer table repeats or omits a claim name at %s", claim.Name)
		}
		if !productionRecoveryServe2AnswerKnown(claim.Answer) {
			t.Fatalf("the claim %s carries the answer %s, which this file does not publish", claim.Name, claim.Answer)
		}
		if len(claim.Evidence) == 0 {
			t.Fatalf("the claim %s carries no evidence", claim.Name)
		}
		named[claim.Name] = claim
		productionRecoveryMarkers(t, claim.Name, claim.Evidence)
	}
	for name, want := range map[string]string{
		productionRecoveryServe2ClaimListBuckets: productionRecoveryServe2EmptyNotNull,
		productionRecoveryServe2ClaimFindNull:    productionRecoveryServe2NullOnAbsent,
		productionRecoveryServe2ClaimMembership:  productionRecoveryServe2StateMembership,
		productionRecoveryServe2ClaimContext:     productionRecoveryServe2OptionalProjection,
		productionRecoveryServe2ClaimCallSite:    productionRecoveryServe2DurableBinder,
		productionRecoveryServe2ClaimCount:       productionRecoveryServe2CountChanged,
	} {
		claim, found := productionRecoveryServe2ClaimFor(name)
		if !found {
			t.Fatalf("the answer table dropped the %s claim", name)
		}
		if claim.Answer != want {
			t.Fatalf("the %s claim now answers %s, want %s", name, claim.Answer, want)
		}
	}
	for _, name := range productionRecoveryServe2SettledClaims() {
		if _, found := productionRecoveryServe2ClaimFor(name); !found {
			t.Fatalf("the settled claim %s is missing", name)
		}
	}
	// The bucket table: two real sibling projections, three empty-by-absence.
	served, absent := 0, 0
	for _, bucket := range productionGenerationRecoveryBuckets() {
		if !productionRecoveryBucketReasonKnown(bucket.Reason) {
			t.Fatalf("the %s bucket carries the reason %s, which this lane does not publish", bucket.Name, bucket.Reason)
		}
		if bucket.Served {
			served++
			if bucket.Reason != productionRecoveryBucketReasonServed {
				t.Fatalf("the served %s bucket is recorded as %s", bucket.Name, bucket.Reason)
			}
			continue
		}
		absent++
		if bucket.Reason != productionRecoveryBucketReasonAbsent {
			t.Fatalf("the %s bucket is empty but recorded as %s", bucket.Name, bucket.Reason)
		}
	}
	if served != 2 || absent != 3 {
		t.Fatalf("the bucket table holds %d served and %d empty buckets, want 2 and 3", served, absent)
	}
}

// TestProductionGenerationRecoveryServe2Wiring proves the two call sites this pass
// needs: the storeful binder outside the owner block, and the owner recorder inside
// it. The ordinals are compared instead of line numbers so the proof survives an
// unrelated edit.
func TestProductionGenerationRecoveryServe2Wiring(t *testing.T) {
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatalf("the wiring file is unreadable: %v", err)
	}
	source := string(raw)
	if count := strings.Count(source, "bindProductionGenerationRecovery("); count != 1 {
		t.Fatalf("the storeful recovery binder is called %d times, want one", count)
	}
	if !productionSeamContains(source, "bindProductionGenerationRecovery(open.durable, server)") {
		t.Fatal("the recovery binder no longer receives the durable open and the server")
	}
	ownerBlock := strings.Index(source, "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {")
	ownerElse := strings.Index(source, "novel-server: production generation owner unavailable")
	ownerBind := strings.Index(source, "bindProductionGenerationRecoveryOwner(owner, server)")
	if ownerBlock < 0 || ownerElse < 0 || ownerBind < 0 {
		t.Fatalf("a wiring declaration this file depends on is gone: owner %d, else %d, owner-bind %d",
			ownerBlock, ownerElse, ownerBind)
	}
	if !(ownerBlock < ownerBind && ownerBind < ownerElse) {
		t.Fatalf("the owner recorder moved out of the owner block: owner %d, owner-bind %d, else %d",
			ownerBlock, ownerBind, ownerElse)
	}
}

// TestProductionGenerationRecoveryServe2Binder checks the answer binder against a
// lane that really binds the two names: it refuses a nil server and a lane missing
// either name, and accepts one that binds both.
func TestProductionGenerationRecoveryServe2Binder(t *testing.T) {
	if err := bindProductionGenerationRecoveryServe2(nil); err == nil ||
		err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the answer binder accepted a nil server: %v", err)
	}
	empty := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := bindProductionGenerationRecoveryServe2(empty); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE2_NAME_MISSING" {
		t.Fatalf("the answer binder accepted a lane that does not serve the names: %v", err)
	}
	bound := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	for _, operation := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if err := bound.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried, so a lane could never install it: %v", operation, err)
		}
	}
	if err := bindProductionGenerationRecoveryServe2(bound); err != nil {
		t.Fatalf("the answer binder refused a lane that serves both names: %v", err)
	}
}
