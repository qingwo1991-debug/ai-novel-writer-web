//go:build linux && amd64

package projectpublish

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// exchangeFixtureDirectories is test-only. Production publication remains
// disabled; this calls the Linux amd64 renameat2 syscall directly so recovery
// classification observes a genuine atomic RENAME_EXCHANGE.
func exchangeFixtureDirectories(t *testing.T, source, stage string) {
	t.Helper()
	sourceParent, err := os.Open(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceParent.Close()
	stageParent, err := os.Open(filepath.Dir(stage))
	if err != nil {
		t.Fatal(err)
	}
	defer stageParent.Close()
	sourceName, err := syscall.BytePtrFromString(filepath.Base(source))
	if err != nil {
		t.Fatal(err)
	}
	stageName, err := syscall.BytePtrFromString(filepath.Base(stage))
	if err != nil {
		t.Fatal(err)
	}
	const sysRenameat2AMD64 = 316
	const renameExchange = 2
	_, _, callErr := syscall.RawSyscall6(sysRenameat2AMD64,
		sourceParent.Fd(), uintptr(unsafe.Pointer(sourceName)),
		stageParent.Fd(), uintptr(unsafe.Pointer(stageName)),
		renameExchange, 0)
	runtime.KeepAlive(sourceName)
	runtime.KeepAlive(stageName)
	if callErr != 0 {
		t.Fatalf("test-only renameat2(RENAME_EXCHANGE): %v", callErr)
	}
}

func TestEditedJournalAtomicExchangeRecoveryAndOldSourceRetention(t *testing.T) {
	f := privateDraftFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Abandon()
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	baseline := "old draft"
	evidence, err := snapshot.EditDraftCASPrivate(ctx, 1, "new private words", 17, &baseline)
	if err != nil {
		t.Fatal(err)
	}
	editedHash := editedStageHash(t, stage)
	journal, err := PrepareEditedExchangeJournal(ctx, snapshot,
		HandleDrainFunc(func(context.Context, string) error { return nil }), evidence)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.root.Close()
	wantOperationHash := hex.EncodeToString(evidence.operationHash[:])
	wantReadbackHash := hex.EncodeToString(evidence.readbackHash[:])
	classify := func(want JournalClass) {
		t.Helper()
		recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot,
			snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
		if err != nil || recovery.Class != want ||
			recovery.EditedDraftID != evidence.DraftID ||
			recovery.EditedOperationHash != wantOperationHash ||
			recovery.EditedProofHash != wantReadbackHash {
			t.Fatalf("recovery class %q: %+v %v", want, recovery, err)
		}
	}
	classify(JournalPrepared)
	exchangeFixtureDirectories(t, f.storage, stage)
	classify(JournalSwitchedUnconfirmed)
	observation, err := snapshot.RetainSwitched(ctx, editedHash)
	if err != nil {
		t.Fatal(err)
	}
	if observation.NewSourcePath != f.storage || observation.OldSourcePath != stage ||
		observation.NewSourceTreeHash != editedHash {
		t.Fatalf("wrong atomic exchange ownership: %+v", observation)
	}
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	classify(JournalSwitched)
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(f.storage, projectfs.DatabaseName): "new private words",
		filepath.Join(stage, projectfs.DatabaseName):     "old draft",
	} {
		read, err := openPrivateDraftDB(path, "ro")
		if err != nil {
			t.Fatal(err)
		}
		var body string
		queryErr := read.QueryRow("SELECT body FROM contents WHERE id=1").Scan(&body)
		closeErr := read.Close()
		if queryErr != nil || closeErr != nil || body != want {
			t.Fatalf("retained tree %s: body=%q query=%v close=%v", path, body, queryErr, closeErr)
		}
	}
}
