//go:build linux

package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The planning population the refusal test needs: one chapter-blueprint-directory
// run whose committed range stops short of the range it requested. It is a
// planning run the reference's own bucket table would collect
// (main-generation-owner.ts:63-67 maps the operation to 'planning'), it is written
// the way the desktop writes it, and the ledger refuses to open the operation here,
// so no writer in this lane could mint one (internal/store/generation_ledger.go:719).
const (
	productionRecoveryServe2PlanningRun       = "ci-recovery-serve2-planning-1"
	productionRecoveryServe2PlanningRoot      = "ci-recovery-serve2-planning-root-1"
	productionRecoveryServe2PlanningOperation = "ci-recovery-serve2-commit-planning"
)

// productionRecoveryServe2Shape is one frozen context a browser could send for the
// same project: its own session, a session naming another project, and this
// project with a lease this lane never minted.
type productionRecoveryServe2Shape struct {
	Name    string
	Context map[string]any
}

// productionRecoveryServe2Fixture names one started lane for a failure message,
// so a failure says which project answered instead of printing a temporary path.
type productionRecoveryServe2Fixture struct {
	Name string
	Lane *productionRecoveryLane
}

// TestProductionGenerationRecoveryServe2Answers re-reads every declaration the
// answer of this pass rests on, then pins the answer itself: an empty bucket is an
// empty array and a missing blueprint recovery is null, neither of them a thrown
// call, and neither of them enough to install a handler while the family's binder
// holds no author database.
//
// The markers are searched for across the whole file rather than only at the
// recorded line: a row that is still documented but has been shifted by an
// unrelated edit is not a regression, while a row that was deleted is.
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

	// Both settlements of the question, each with the answer this pass proved.
	for name, want := range map[string]string{
		productionRecoveryServe2ClaimListBuckets: productionRecoveryServe2EmptyNotNull,
		productionRecoveryServe2ClaimFindNull:    productionRecoveryServe2NullOnAbsent,
		productionRecoveryServe2ClaimMembership:  productionRecoveryServe2StateMembership,
		productionRecoveryServe2ClaimContext:     productionRecoveryServe2OptionalProjection,
		productionRecoveryServe2ClaimCallSite:    productionRecoveryServe2StorelessBinder,
		productionRecoveryServe2ClaimCount:       productionRecoveryServe2CountUnchanged,
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
	// The find answer has to rest on both halves: the nullable signature and the
	// null return. A signature alone would prove the type, not the behaviour.
	find, _ := productionRecoveryServe2ClaimFor(productionRecoveryServe2ClaimFindNull)
	signature, returned := false, false
	for _, line := range find.Evidence {
		if strings.Contains(line.Marker, "GenerationRecoveryContext | null => {") {
			signature = true
		}
		if strings.HasSuffix(line.Marker, "return null") {
			returned = true
		}
	}
	if !signature || !returned {
		t.Fatalf("the null answer is not grounded: signature %v, null return %v", signature, returned)
	}

	// The bucket table says which buckets are real, and the two real ones are the
	// siblings' projections, not empty placeholders.
	served, state := 0, 0
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
		if bucket.Name == "planning" {
			state++
			if bucket.Reason != productionRecoveryBucketReasonState {
				t.Fatalf("the planning bucket is the state-driven one, recorded as %s", bucket.Reason)
			}
			continue
		}
		if bucket.Reason != productionRecoveryBucketReasonPort {
			t.Fatalf("the %s bucket is refused for a missing port, recorded as %s", bucket.Name, bucket.Reason)
		}
	}
	if served != 2 || state != 1 {
		t.Fatalf("the bucket table holds %d served and %d state-driven buckets, want 2 and 1", served, state)
	}
}

// TestProductionGenerationRecoveryServe2WiringGap proves the obstacle this pass
// found, from the wiring file itself: the recovery binder is one closure among the
// read binders, the author database and the owner are both in reach of its
// neighbours, and it receives neither. The ordinals are compared instead of line
// numbers so the proof survives an unrelated edit; a real move that puts the
// binder inside the owner block fails here, which is exactly the change a later
// pass has to make before serving either name.
func TestProductionGenerationRecoveryServe2WiringGap(t *testing.T) {
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatalf("the wiring file is unreadable: %v", err)
	}
	source := string(raw)
	// Exactly one call, and it passes the server alone: a call that also passed
	// the author runtime would make this file's claim stale.
	if count := strings.Count(source, "bindProductionGenerationRecovery("); count != 1 {
		t.Fatalf("the recovery binder is called %d times, want the one call this file describes", count)
	}
	if !productionSeamContains(source, "bindProductionGenerationRecovery(server)") {
		t.Fatal("the recovery binder no longer receives the server alone")
	}
	for _, widened := range []string{"bindProductionGenerationRecovery(open", "bindProductionGenerationRecovery(durable"} {
		if strings.Contains(source, widened) {
			t.Fatalf("the recovery binder now receives more than the server: %s", widened)
		}
	}
	ownerBlock := strings.Index(source, "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {")
	editorBinder := strings.Index(source, "newProductionEditorInline(owner).Bind(server)")
	ownerElse := strings.Index(source, "novel-server: production generation owner unavailable")
	durableForm := strings.Index(source, "newProductionBatchReads(open.durable).Bind(server)")
	recoveryCall := strings.Index(source, "bindProductionGenerationRecovery(server)")
	if ownerBlock < 0 || editorBinder < 0 || ownerElse < 0 || durableForm < 0 || recoveryCall < 0 {
		t.Fatalf("a wiring declaration this file depends on is gone: owner %d, editors %d, else %d, batches %d, recovery %d",
			ownerBlock, editorBinder, ownerElse, durableForm, recoveryCall)
	}
	// The editors bucket's binder lives inside the owner block ...
	if !(ownerBlock < editorBinder && editorBinder < ownerElse) {
		t.Fatalf("the editor-inline binder moved out of the owner block: owner %d, editors %d, else %d",
			ownerBlock, editorBinder, ownerElse)
	}
	// ... while the recovery binder runs after the block closed, in the same list
	// as the closure that passes the author runtime - and passes only the server.
	if !(ownerElse < durableForm && durableForm < recoveryCall) {
		t.Fatalf("the recovery binder no longer follows the durable-form closure: else %d, batches %d, recovery %d",
			ownerElse, durableForm, recoveryCall)
	}
}

// TestProductionGenerationRecoveryServe2RefusesWithAndWithoutPlanningRun is the
// answer as the browser sees it. Two projects: one really holds a resumable
// chapter-blueprint-directory run - a planning run, proved through the SERVED
// directory catalog that reads the same rows - and one holds no planning run at
// all. Both answer both names with the same 501 bytes for every frozen context
// shape, which is the point: the reference answers null/empty when there is
// nothing to recover, and this lane cannot tell "nothing to recover" from "not
// projected", so it refuses both projects instead of answering the second one with
// an empty bucket it would have to fill for the first.
func TestProductionGenerationRecoveryServe2RefusesWithAndWithoutPlanningRun(t *testing.T) {
	withPlanning := productionRecoveryOpen(t, func(t *testing.T, database string) {
		productionRecoverySeedDirectory(t, database, []productionRecoveryDirectorySeed{{
			RunID:       productionRecoveryServe2PlanningRun,
			RootID:      productionRecoveryServe2PlanningRoot,
			ProjectID:   productionEditorProjectID,
			OperationID: productionRecoveryServe2PlanningOperation,
			Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
			Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
		}})
	})
	withoutPlanning := productionRecoveryOpen(t, nil)
	fixtures := []productionRecoveryServe2Fixture{
		{Name: "project with a planning run", Lane: withPlanning},
		{Name: "project without a planning run", Lane: withoutPlanning},
	}

	// The population, not the projection: the seeded run is a directory run the
	// served catalog lists as resumable, and the other project has none.
	var listed productionRecoveryCatalogView
	withPlanning.client.decode(t, productionGenerationRecoveryDirectory, []any{withPlanning.frozen}, &listed)
	if len(listed.Progress) != 1 || listed.Progress[0].OperationID != productionRecoveryServe2PlanningOperation {
		t.Fatalf("the seeded planning run is not in the served catalog: %d receipts", len(listed.Progress))
	}
	if listed.Progress[0].RemainingRange == nil {
		t.Fatal("the seeded planning run has nothing left to write, so it is not the state this test is about")
	}
	var empty productionRecoveryCatalogView
	withoutPlanning.client.decode(t, productionGenerationRecoveryDirectory, []any{withoutPlanning.frozen}, &empty)
	if len(empty.Progress) != 0 || len(empty.Diagnostics) != 0 {
		t.Fatalf("the project without a planning run reported %d receipts and %d diagnostics",
			len(empty.Progress), len(empty.Diagnostics))
	}

	before := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		before[fixture.Name] = productionMCPDatabaseDigest(t, fixture.Lane.projectPath)
	}
	// Both refused names, both projects, every context shape: one answer.
	for _, operation := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		first, firstWhere := "", ""
		for _, fixture := range fixtures {
			for _, shape := range []productionRecoveryServe2Shape{
				{Name: "own project", Context: fixture.Lane.frozen},
				{Name: "foreign project", Context: map[string]any{
					"projectId":   productionRecoveryForeignProject,
					"projectPath": fixture.Lane.projectPath,
					"leaseId":     fixture.Lane.frozen["leaseId"],
				}},
				{Name: "stale lease", Context: map[string]any{
					"projectId":   fixture.Lane.frozen["projectId"],
					"projectPath": fixture.Lane.projectPath,
					"leaseId":     productionRecoveryServeStaleLease,
				}},
			} {
				response := fixture.Lane.client.call(t, operation, []any{shape.Context})
				if response.Code != http.StatusNotImplemented ||
					!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
					t.Fatalf("%s over the %s with a %s session = %d %s, want 501 GO_OPERATION_NOT_MIGRATED",
						operation, fixture.Name, shape.Name, response.Code, response.Body.String())
				}
				where := fixture.Name + " with a " + shape.Name + " session"
				if first == "" {
					first, firstWhere = response.Body.String(), where
					continue
				}
				if response.Body.String() != first {
					t.Fatalf("%s answered the %s with %s but the %s with %s; a refused name has one answer",
						operation, where, response.Body.String(), firstWhere, first)
				}
			}
		}
	}
	for _, fixture := range fixtures {
		if after := productionMCPDatabaseDigest(t, fixture.Lane.projectPath); after != before[fixture.Name] {
			t.Fatalf("a refused recovery moved a byte of the author database of the %s", fixture.Name)
		}
	}
}

// TestProductionGenerationRecoveryServe2Binder checks the answer binder against the
// lane it runs in: it installs nothing, it refuses a nil server, it refuses a lane
// that already installs either name - the tripwire that makes this file's call-site
// claim expire with the change that serves them - and the family's own binder, which
// is that call site, reaches it.
func TestProductionGenerationRecoveryServe2Binder(t *testing.T) {
	if err := bindProductionGenerationRecoveryServe2(nil); err == nil ||
		err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the answer binder accepted a nil server: %v", err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := bindProductionGenerationRecoveryServe2(server); err != nil {
		t.Fatalf("the answer binder refused an empty lane: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the answer binder installed %v, want nothing", names)
	}
	for _, operation := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried, so a lane could never install it: %v", operation, err)
		}
		if err := bindProductionGenerationRecoveryServe2(fresh); err == nil ||
			err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE2_STALE_CLAIM" {
			t.Fatalf("the answer binder accepted a lane that installs %s: %v", operation, err)
		}
	}
	// The family binder is the call site this file describes, so it has to reach
	// the answer binder and still install neither name.
	lane := productionRecoveryOpen(t, nil)
	before := len(lane.server.BoundOperations())
	if err := bindProductionGenerationRecovery(lane.server); err != nil {
		t.Fatalf("the family binder refused the finished lane: %v", err)
	}
	after := lane.server.BoundOperations()
	if len(after) != before {
		t.Fatalf("the family binder installed %d operations, want none", len(after)-before)
	}
	for _, operation := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if productionRecoveryBoundContains(after, operation) {
			t.Fatalf("%s is bound although this file records it as refused", operation)
		}
	}
}
