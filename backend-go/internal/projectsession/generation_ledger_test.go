package projectsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)

// This internal integration deliberately registers no renderer operation: a scratch
// lease is not a generation owner, provider usage proof, or durable author save.
func TestGenerationLedgerPrivateSessionLifecycleDoesNotPublishSource(t *testing.T) {
	project, contract, source := sessionFixture(t)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	manager := New()
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	owner := Owner{"ledger-cookie", "ledger-tab"}
	scratch := t.TempDir()
	lease, err := manager.OpenScratchCopy(ctx, owner, project, contract, scratch, 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []Owner{{"foreign-cookie", owner.ClientID}, {owner.SessionID, "foreign-tab"}} {
		if _, done, err := manager.BeginGenerationLedger(ctx, foreign, lease); err == nil {
			done()
			t.Fatal("foreign ledger borrower")
		}
	}
	repo, done, err := manager.BeginGenerationLedger(ctx, owner, lease)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method string, args any) any {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		value, err := repo.CallJSON(ctx, method, raw)
		if err != nil {
			t.Fatal(method, err)
		}
		return value
	}
	text := strings.Repeat("a", 64)
	// Fixed JSON field order is part of the original fingerprint/budget replay key.
	request := fmt.Sprintf(`[{"projectId":%q,"epoch":"synthetic-runtime-epoch","fingerprint":{"chapterBriefHash":%q,"authorGuidanceHash":%q,"dependencyHash":%q,"contextSnapshotHash":%q,"templateHash":%q,"skillSnapshotHash":%q,"modelLeaseRevision":%q,"policyHash":%q,"outputContractHash":%q},"contextSnapshotId":"synthetic-context","sourceManifest":{"purpose":"test-only"},"sourceRefs":[],"operation":"draft","uiActionNonce":"synthetic-open","frozenInputHash":%q,"budget":{"maxPhysicalRequests":5,"maxTokenLiability":100,"maxOutputPerRequest":50,"maxActiveElapsedMs":60000}}]`, lease.ProjectID, text, text, text, text, text, text, text, text, text, text)
	value, err := repo.CallJSON(ctx, "open", json.RawMessage(request))
	if err != nil {
		t.Fatal(err)
	}
	opened := value.(map[string]any)
	run := opened["runId"].(string)
	root := opened["rootActionId"].(string)
	receipt := call("reserve", []any{run, "synthetic-invocation", text, 100, 40}).(map[string]any)
	attempt := receipt["attempt"].(map[string]any)["attemptId"].(string)
	call("markDispatched", []any{attempt})
	prose := "合成生成正文🙂\n永不发布原书"
	artifact := call("snapshot", []any{attempt, 0, prose}).(map[string]any)
	if artifact["text"] != prose {
		t.Fatal(artifact)
	}
	if !errors.Is(manager.Release(owner, lease), novelruntime.ErrBusy) {
		t.Fatal("released inflight ledger lease")
	}
	if !errors.Is(manager.Close(), novelruntime.ErrBusy) {
		t.Fatal("closed inflight ledger")
	}
	done()
	done()
	same, err := manager.OpenScratchCopy(ctx, owner, project, contract, scratch, 32*1024*1024)
	if err != nil || same != lease {
		t.Fatal(same, err)
	}
	repo, done, err = manager.BeginGenerationLedger(ctx, owner, same)
	if err != nil {
		t.Fatal(err)
	}
	// Conservative owner restart classification: dispatched becomes unknown, never a
	// fictitious trusted usage receipt, and its durable visible prefix survives.
	call("recoverInterrupted", []any{})
	recovered := call("receipt", []any{attempt}).(map[string]any)
	if recovered["attempt"].(map[string]any)["status"] != "unknown" || recovered["artifact"].(map[string]any)["text"] != prose {
		t.Fatal(recovered)
	}
	if call("get", []any{run}).(map[string]any)["status"] != "paused" {
		t.Fatal("restart did not pause")
	}
	budget := call("budget", []any{root}).(map[string]any)
	if budget["root"].(map[string]any)["status"] != "paused" {
		t.Fatal(budget)
	}
	done()
	if err := manager.Release(owner, lease); err != nil {
		t.Fatal(err)
	}
	if _, done, err := manager.BeginGenerationLedger(ctx, owner, lease); err == nil {
		done()
		t.Fatal("revoked lease lent ledger")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source SQLite changed", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("scratch remains", entries, err)
	}
}
