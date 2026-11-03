//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func editedProof() EditedDraftCASProof {
	op := sha256.Sum256([]byte("synthetic draft CAS intent"))
	readback := sha256.Sum256([]byte("synthetic private SQLite readback"))
	return EditedDraftCASProof{DraftID: 7, OperationHash: hex.EncodeToString(op[:]), ReadbackHash: hex.EncodeToString(readback[:])}
}

func TestEditedJournalBindsOnlySQLiteStageAndTrustedProof(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	stagePath, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagePath, projectfs.DatabaseName), []byte("synthetic edited private DB"), 0600); err != nil {
		t.Fatal(err)
	}
	proofCalls, drainCalls := 0, 0
	journal, err := PrepareEditedExchangeJournal(ctx, snapshot,
		HandleDrainFunc(func(_ context.Context, path string) error {
			drainCalls++
			if path != f.projectPath {
				t.Fatalf("drain given foreign project %q", path)
			}
			return nil
		}), EditedDraftCASVerifierFunc(func(_ context.Context, facts EditedStageFacts) (EditedDraftCASProof, error) {
			proofCalls++
			if facts.ProjectID != snapshot.projectID || facts.ProjectPath != f.projectPath || facts.StagePath != stagePath ||
				!reflect.DeepEqual(facts.ChangedSQLite, []string{projectfs.DatabaseName}) ||
				!validJournalHash(facts.SourceTreeHash) || !validJournalHash(facts.EditedTreeHash) || facts.SourceTreeHash == facts.EditedTreeHash {
				t.Fatalf("edited stage facts incomplete: %+v", facts)
			}
			return editedProof(), nil
		}))
	if err != nil || journal == nil || proofCalls != 1 || drainCalls != 1 {
		t.Fatalf("edited PREPARED marker failed: %+v %v calls=%d/%d", journal, err, proofCalls, drainCalls)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalPrepared || recovery.EditedDraftID != editedProof().DraftID ||
		recovery.EditedOperationHash != editedProof().OperationHash || recovery.EditedProofHash != editedProof().ReadbackHash {
		t.Fatalf("edited marker did not bind draft CAS proof: %+v %v", recovery, err)
	}
	after, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("edited journal changed author source: %v", err)
	}
	if err := journal.MarkSwitched(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("edited stage marker claimed a switch: %v", err)
	}
	// Only the test moves disposable directory names to prove restart
	// classification with different source and candidate digests.
	spare := filepath.Join(f.dataRoot, "synthetic-edited-old-storage")
	for _, move := range [][2]string{{f.storage, spare}, {stagePath, f.storage}, {spare, stagePath}} {
		if err := os.Rename(move[0], move[1]); err != nil {
			t.Fatal(err)
		}
	}
	recovery, err = RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalSwitchedUnconfirmed {
		t.Fatalf("edited stage swap not recognized: %+v %v", recovery, err)
	}
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	recovery, err = RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalSwitched || recovery.EditedDraftID != editedProof().DraftID {
		t.Fatalf("edited stage SWITCHED marker not recognized: %+v %v", recovery, err)
	}
	_ = snapshot.parent.Close()
}

func TestEditedJournalRejectsUnknownEditsAndChangedProof(t *testing.T) {
	for _, change := range []string{"unknown-asset", "verifier-refusal", "verifier-mutates-stage", "verifier-mutates-source", "bad-proof", "nil-verifier"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			stagePath, err := snapshot.PrivatePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stagePath, projectfs.DatabaseName), []byte("edited DB"), 0600); err != nil {
				t.Fatal(err)
			}
			if change == "unknown-asset" {
				if err := os.WriteFile(filepath.Join(stagePath, "unknown-author-file.bin"), []byte("changed unknown asset"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			verifier := EditedDraftCASVerifierFunc(func(context.Context, EditedStageFacts) (EditedDraftCASProof, error) {
				switch change {
				case "verifier-refusal":
					return EditedDraftCASProof{}, ErrPublishUnavailable
				case "verifier-mutates-stage":
					if err := os.WriteFile(filepath.Join(stagePath, projectfs.DatabaseName), []byte("second edit"), 0600); err != nil {
						return EditedDraftCASProof{}, err
					}
				case "verifier-mutates-source":
					if err := os.WriteFile(filepath.Join(f.storage, projectfs.DatabaseName), []byte("source changed"), 0600); err != nil {
						return EditedDraftCASProof{}, err
					}
				case "bad-proof":
					return EditedDraftCASProof{DraftID: 7, OperationHash: "bad", ReadbackHash: "bad"}, nil
				}
				return editedProof(), nil
			})
			if change == "nil-verifier" {
				verifier = nil
			}
			journal, err := PrepareEditedExchangeJournal(ctx, snapshot, HandleDrainFunc(func(context.Context, string) error { return nil }), verifier)
			if journal != nil || err == nil {
				t.Fatalf("unqualified edit created journal: %+v %v", journal, err)
			}
		})
	}
}

func TestEditedSQLiteDeltaAllowsOnlyExactDatabaseArtifacts(t *testing.T) {
	source := []entry{
		{path: projectfs.DatabaseName, mode: 0600, size: 2},
		{path: projectfs.DatabaseName + "-wal", mode: 0600, size: 3},
		{path: "unknown-author-file.bin", mode: 0644, size: 4},
		{path: "lancedb", mode: fs.ModeDir | 0700},
	}
	good := []entry{
		{path: projectfs.DatabaseName, mode: 0600, size: 5},
		{path: projectfs.DatabaseName + "-shm", mode: 0600, size: 1},
		{path: "unknown-author-file.bin", mode: 0644, size: 4},
		{path: "lancedb", mode: fs.ModeDir | 0700},
	}
	changed, err := editedSQLiteOnly(source, good)
	if err != nil || !reflect.DeepEqual(changed, []string{projectfs.DatabaseName, projectfs.DatabaseName + "-shm", projectfs.DatabaseName + "-wal"}) {
		t.Fatalf("bounded SQLite delta refused: %v %+v", err, changed)
	}
	for _, bad := range []struct {
		name  string
		items []entry
	}{
		{"unknown-file", append(append([]entry{}, good...), entry{path: "extra.bin", mode: 0600, size: 1})},
		{"missing-main", good[1:]},
		{"unsafe-new-sidecar-mode", []entry{{path: projectfs.DatabaseName, mode: 0600, size: 5}, {path: projectfs.DatabaseName + "-shm", mode: 0644, size: 1}, source[2], source[3]}},
		{"changed-directory-mode", []entry{good[0], good[1], good[2], {path: "lancedb", mode: fs.ModeDir | 0755}}},
		{"identical", source},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if paths, err := editedSQLiteOnly(source, bad.items); err == nil {
				t.Fatalf("unsafe stage delta accepted: %+v", paths)
			}
		})
	}
}
