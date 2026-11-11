//go:build linux

package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// This file is the multi-tab contract of the production lane: "any number of
// browser tabs can open and use the SAME project at the same time".
//
// It pins the two identities the lane now keeps apart, on synthetic fixtures
// only:
//
//   - the LEASE OWNER is the authenticated ACCOUNT (productionLeaseOwner), so
//     every tab of one author resolves the same owner and ADOPTS the single live
//     project lease instead of being refused with PROJECT_OPEN_IN_ANOTHER_TAB;
//   - the TAB IDENTITY is the cookie session plus the browser tab
//     (productionTabOwner), which still scopes file grants, staged uploads and
//     generation/import-run receipts, so a sibling tab cannot reach the other
//     tab's staged bytes or model lease.
//
// Write safety between two tabs of one account is NOT the lease: it is the
// per-resource baseline the store already enforces. The draft body's
// expectedContent CAS is asserted here, and a genuinely different account is
// still refused, which is the security boundary the sharing must not cross.

// TestProductionMultiTabOpensTheSameProjectFromEveryTab proves the open half:
// several tab identities of one account all open one project successfully, all
// resolve the SAME lease, all read the same committed state, and none of them
// disturbs the first tab. A different account is still refused, and refusing it
// leaves the account that holds the lease working.
func TestProductionMultiTabOpensTheSameProjectFromEveryTab(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Four tabs of one account. The tab identity is the only thing that differs:
	// same principal, same project, distinct cookie sessions and X-Client-Id.
	tabs := []string{productionDurableTab, productionDurableOtherTab, "client_1122334455", "client_5544332211"}
	clients := make([]*productionDurableClient, 0, len(tabs))
	lease := ""
	for index, tab := range tabs {
		client := newProductionDurableClient(t, handler, tab)
		var opened productionDurableOpenWire
		client.decode(t, "project:open", []any{projectPath, "open-token-" + tab, projectPath}, &opened)
		if !opened.Success || !opened.DBReady || opened.Error != "" {
			t.Fatalf("tab %d (%s) of the owning account was refused the project: %+v", index, tab, opened)
		}
		openedLease, _ := opened.Project["sessionLease"].(string)
		if openedLease == "" {
			t.Fatalf("tab %d (%s) opened without a session lease: %v", index, tab, opened.Project)
		}
		if lease == "" {
			lease = openedLease
		} else if openedLease != lease {
			t.Fatalf("tab %d (%s) was handed a different lease for the shared project: %q vs %q",
				index, tab, openedLease, lease)
		}
		clients = append(clients, client)
	}

	frozen := map[string]any{
		"projectId":   productionDurableProjectID,
		"projectPath": projectPath,
		"leaseId":     lease,
	}
	// Every tab reads the same committed draft under the one shared lease, the
	// first one included: the later opens did not take the project away from it.
	for index, client := range clients {
		var full productionDurableDraftWire
		client.decode(t, "db:draft-get-full", []any{int64(1), projectPath, frozen}, &full)
		if full.Content != productionDurableSeed || full.ID != 1 {
			t.Fatalf("tab %d read %+v; want the one committed draft", index, full)
		}
	}

	// A genuinely different ACCOUNT is refused the lease, and the refusal is a
	// non-event for the account that holds it.
	stranger := newProductionDurableClientAs(t, handler, "other-author", productionDurableOtherTab)
	var cross productionDurableOpenWire
	stranger.decode(t, "project:open", []any{projectPath, "open-token-stranger", projectPath}, &cross)
	if cross.Success || cross.Error != "PROJECT_OPEN_IN_ANOTHER_TAB" {
		t.Fatalf("a different account was not refused the project lease: %+v", cross)
	}
	var still productionDurableDraftWire
	clients[0].decode(t, "db:draft-get-full", []any{int64(1), projectPath, frozen}, &still)
	if still.Content != productionDurableSeed {
		t.Fatalf("refusing another account disturbed the owning account's read: %+v", still)
	}
}

// TestProductionMultiTabDraftCASLosesExactlyOneSave proves the write half: two
// tabs of one account hold the same lease, and the draft body's expectedContent
// CAS - not the lease - decides the winner. The loser is refused with
// DRAFT_CONTENT_CONFLICT, the winner's bytes stay on disk, and the losing tab can
// still save once it reads the winner's body, so the conflict is a lost update
// and not a dead tab.
func TestProductionMultiTabDraftCASLosesExactlyOneSave(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	first := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	first.decode(t, "project:open", []any{projectPath, "open-token-first", projectPath}, &opened)
	if !opened.Success || opened.Error != "" {
		t.Fatalf("the first tab was refused the project: %+v", opened)
	}
	second := newProductionDurableClient(t, handler, productionDurableOtherTab)
	var sibling productionDurableOpenWire
	second.decode(t, "project:open", []any{projectPath, "open-token-second", projectPath}, &sibling)
	if !sibling.Success || sibling.Error != "" {
		t.Fatalf("the second tab was refused the project: %+v", sibling)
	}
	// Both tabs are on the SAME lease: this is what makes the CAS, not the lease,
	// the thing that resolves their collision.
	if sibling.Project["sessionLease"] != opened.Project["sessionLease"] {
		t.Fatalf("two tabs of one account hold different leases: %v vs %v",
			sibling.Project["sessionLease"], opened.Project["sessionLease"])
	}
	baseline := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}

	// The first tab wins with the body it read.
	const winner = "第一章改稿：她推开门。"
	var saved productionDurableSaveWire
	first.decode(t, "db:draft-update-content", []any{int64(1), winner, int64(12), projectPath, productionDurableSeed, baseline}, &saved)
	if !saved.Success || saved.Error != "" {
		t.Fatalf("the first tab could not save the body it read: %+v", saved)
	}
	if body := productionDurableBody(t, projectPath); body != winner {
		t.Fatalf("the winning tab's body did not persist: %q", body)
	}

	// The second tab must NOT need a project:open refresh after the first tab's
	// acknowledged save rotated the shared lease. Current reports this tab's own
	// view, and the next save reaches the CAS with the old frozen context.
	var currentContext map[string]any
	second.decode(t, "project:get-runtime-context", []any{}, &currentContext)
	if currentContext["dbReady"] != true || currentContext["leaseId"] != baseline["leaseId"] {
		t.Fatalf("a sibling tab was shown a lease it never saw: %+v", currentContext)
	}
	current := map[string]any{
		"projectId":   baseline["projectId"],
		"projectPath": baseline["projectPath"],
		"leaseId":     baseline["leaseId"],
	}
	var lost productionDurableSaveWire
	second.decode(t, "db:draft-update-content", []any{int64(1), "第三章：抢写。", int64(6), projectPath, productionDurableSeed, current}, &lost)
	if lost.Success || lost.ErrorCode != "DRAFT_CONTENT_CONFLICT" {
		t.Fatalf("a sibling tab's stale baseline was not a CAS conflict: %+v", lost)
	}
	if body := productionDurableBody(t, projectPath); body != winner {
		t.Fatalf("the refused sibling save still changed the database: %q", body)
	}

	// Deterministic and recoverable: the losing tab saves the moment it reads the
	// winner's body, so the conflict costs one save, never the tab.
	const follow = "第一章再改：他推开门。"
	var accepted productionDurableSaveWire
	second.decode(t, "db:draft-update-content", []any{int64(1), follow, int64(12), projectPath, winner, current}, &accepted)
	if !accepted.Success || accepted.Error != "" {
		t.Fatalf("the losing tab could not save against the winner's body: %+v", accepted)
	}
	if body := productionDurableBody(t, projectPath); body != follow {
		t.Fatalf("the recovered save did not persist: %q", body)
	}
}

// TestProductionMultiTabGenerationReceiptStaysScopedToItsTab is the receipt half
// and the sharpest one: a sibling tab really does share the account lease - its
// execute gets past the project fence and the run's own project/epoch proof - and
// is then refused by the MODEL LEASE REGISTRY, because the run's execution lease
// was minted under the other tab's identity. If the receipt scope had been keyed
// by the shared account owner, that refusal could not happen and one tab's model
// lease would be resolvable by every tab of the account.
func TestProductionMultiTabGenerationReceiptStaysScopedToItsTab(t *testing.T) {
	// The dispatch seam is the only thing that answers a provider call, so a tab
	// that was wrongly served would be visible both here and in the byte
	// comparison below.
	seam := &productionGenerationSeam{text: "多标签生成：门后有人。"}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)

	// One run, opened and owned by the fixture's tab.
	handle := lane.begin(t, productionEditorBeginRequest(productionGenerationWriteBeginNonce))
	afterBegin := lane.bytes(t)

	// A second tab of the SAME account opens the same project. It shares the lease,
	// which is the whole point: it is not refused at the open or at the borrow.
	sibling := newProductionDurableClient(t, lane.author.handler, productionEditorOtherTab)
	var opened productionDurableOpenWire
	sibling.decode(t, "project:open", []any{lane.projectPath, "open-token-multitab", lane.projectPath}, &opened)
	if !opened.Success || opened.Error != "" {
		t.Fatalf("a sibling tab was refused the shared project: %+v", opened)
	}
	if opened.Project["sessionLease"] != lane.frozen["leaseId"] {
		t.Fatalf("the sibling tab did not adopt the live lease: %v vs %v",
			opened.Project["sessionLease"], lane.frozen["leaseId"])
	}

	// The sibling tab tries to spend the other tab's run. It clears the project
	// fence, and the registry refuses the lease it did not mint.
	response := sibling.call(t, "generation:execute",
		[]any{productionExecuteRequest(handle, "production-multitab-invoke"), lane.frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:execute is unbound in the production lane: %d %s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":false`) {
		t.Fatalf("a sibling tab's cross-tab execute was not refused through the business envelope: %d %s",
			response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "MODEL_EXECUTION_LEASE_INVALID") {
		t.Fatalf("a sibling tab was refused by something other than the receipt scope: %s", response.Body.String())
	}
	if seam.dispatched() != 0 {
		t.Fatalf("a cross-tab execute dialed the provider %d times", seam.dispatched())
	}
	if !bytes.Equal(lane.bytes(t), afterBegin) {
		t.Fatal("a refused cross-tab generation:execute changed the committed author database")
	}

	// The refusal was the SCOPE and nothing else: the tab that minted the lease
	// still spends its own run, which is what makes the sibling's refusal an
	// isolation proof instead of a broken feature.
	answer := productionExecuteAnswer(t, lane.author,
		productionExecuteRequest(handle, "production-multitab-owner-invoke"), lane.frozen)
	outcome, ok := answer["outcome"].(map[string]any)
	if !ok || outcome["status"] != "completed" {
		t.Fatalf("the tab that minted the receipt was refused its own run: %v", answer)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("the owning tab's execute dispatched %d times; want exactly one", seam.dispatched())
	}
}
