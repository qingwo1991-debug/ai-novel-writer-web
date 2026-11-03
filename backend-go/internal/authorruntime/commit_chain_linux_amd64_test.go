//go:build linux && amd64

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// This is a synthetic integration fixture for the unbound commit primitives.
// It does not exercise the browser save route or grant it a success response.
func TestSyntheticDraftCommitChainRetainsOldSourceAndInstallsCatalogBackedLease(t *testing.T) {
	_, _, config, source, _ := readFixture(t)
	ctx := context.Background()
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	content, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "old draft")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := content.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	draft, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id,word_count) VALUES(?,1,'draft',?,9)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := draft.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	oldDatabase, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(config.DataRoot, "private-publish")
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	var journalName string
	readback := func(ctx context.Context, path, id string, recovery projectpublish.JournalRecovery) error {
		if journalName == "" || path != config.ProjectPath || id != fixtureProjectID() || recovery.EditedDraftID != draftID {
			return ErrSwitchedDraftUnverified
		}
		_, err := VerifyRestartedSwitchedDraftUnderLock(ctx, held, library, id, path, stageParent, journalName, config)
		return err
	}
	check, err := CatalogJournalRecoveryCheck(ctx, held, config.DataRoot, stageParent, readback)
	if err != nil {
		t.Fatal(err)
	}
	metadata := t.TempDir()
	if err := os.Chmod(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := projectcatalog.OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		t.Fatal(err)
	}
	project, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := catalog.Register(project)
	if closeErr := project.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewReadLifecycleWithHeldLock(catalog, configWithoutProject(config), held)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	defer func() {
		if err := lifecycle.Shutdown(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	owner := projectsession.Owner{SessionID: "synthetic-cookie", ClientID: "synthetic-tab"}
	oldLease, err := lifecycle.Open(ctx, owner, initial.Key, "initial-open")
	if err != nil {
		t.Fatal(err)
	}
	const changed = "new private words"
	var retainedPath, newKey string
	newLease, err := lifecycle.active.CommitDrained(ctx, owner, oldLease, initial.Key,
		func(ctx context.Context, held *writerlock.Lock, fence *projectsession.PublishFence) (projectsession.Admitted, projectsession.CommitClearance, error) {
			if err := catalog.HoldRecovery(config.ProjectPath, fixtureProjectID()); err != nil {
				return nil, nil, err
			}
			snapshot, err := projectpublish.Stage(ctx, held, config.DataRoot, library, config.ProjectPath, stageParent,
				projectpublish.Limits{MaxFiles: 1000, MaxBytes: 256 << 20, MaxDepth: 16})
			if err != nil {
				return nil, nil, err
			}
			defer snapshot.Close()
			retainedPath, err = snapshot.PrivatePath()
			if err != nil {
				return nil, nil, err
			}
			baseline := "old draft"
			evidence, err := snapshot.EditDraftCASPrivate(ctx, draftID, changed, 17, &baseline)
			if err != nil {
				return nil, nil, err
			}
			claim := projectpublish.EditedExchangeClaim{Held: held, Fence: fence, Owner: owner, Expected: oldLease, OldKey: initial.Key}
			current := projectpublish.CurrentDraftAuthorityFunc(func(ctx context.Context, facts projectpublish.CurrentDraftFacts) error {
				if facts.ProjectID != oldLease.ProjectID || facts.ProjectPath != oldLease.ProjectPath || facts.DraftID != draftID {
					return ErrChanged
				}
				return fence.VerifyCommit(ctx, held, owner, oldLease, initial.Key)
			})
			drain := projectpublish.HandleDrainFunc(func(ctx context.Context, path string) error {
				if path != oldLease.ProjectPath {
					return ErrChanged
				}
				return fence.VerifyCommit(ctx, held, owner, oldLease, initial.Key)
			})
			journal, err := projectpublish.PrepareEditedExchangeJournal(ctx, snapshot, drain, evidence)
			if err != nil {
				return nil, nil, err
			}
			journalName = journal.Name()
			plan, err := projectpublish.PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current, drain)
			if err != nil {
				return nil, nil, err
			}
			if err := plan.ExchangeWithFence(ctx, claim); err != nil {
				return nil, nil, err
			}
			editedHash := hex.EncodeToString(evidence.AfterStageTreeSHA256[:])
			if _, err := snapshot.RetainSwitched(ctx, editedHash); err != nil {
				return nil, nil, err
			}
			if err := journal.MarkSwitched(ctx); err != nil {
				return nil, nil, err
			}
			recovery, err := projectpublish.RecoverExchangeJournal(ctx, held, config.DataRoot, fixtureProjectID(), config.ProjectPath, stageParent, journalName)
			if err != nil || recovery.Class != projectpublish.JournalSwitched {
				return nil, nil, errors.Join(ErrSwitchedDraftUnverified, err)
			}
			expected := SwitchedDraftExpectation{ProjectID: fixtureProjectID(), DraftID: draftID, Content: changed,
				WordCount: 17, EditedTreeHash: editedHash, EditedOperationHash: recovery.EditedOperationHash}
			if _, err := VerifySwitchedDraftUnderLock(ctx, held, library, snapshot, config.ProjectPath, stageParent, journalName, config, expected); err != nil {
				return nil, nil, err
			}
			if err := snapshot.Close(); err != nil {
				return nil, nil, err
			}
			pinned, err := library.OpenProject(config.ProjectPath)
			if err != nil {
				return nil, nil, err
			}
			reader, err := openReadOnlyPinnedProjectUnderLock(ctx, held, pinned, config)
			if err != nil {
				return nil, nil, err
			}
			candidate := &admittedRead{reader: reader, context: projectsession.Context{
				ProjectID: oldLease.ProjectID, ProjectPath: oldLease.ProjectPath, LeaseID: "synthetic-fresh-lease"}}
			return candidate, func(ctx context.Context, admitted projectsession.Admitted) (string, error) {
				if admitted != candidate {
					return "", ErrInvalid
				}
				if _, done, err := lifecycle.active.Begin(ctx, owner, candidate.context); !errors.Is(err, runtime.ErrBusy) {
					if done != nil {
						done()
					}
					return "", errors.Join(ErrInvalid, err)
				}
				promoted, err := catalog.PromoteVerifiedRecoveryWithBinding(ctx, reader.project,
					func(key string, verified *projectfs.Project) (func(), error) {
						return reader.bindPromotedCatalogKey(ctx, catalog, key, verified)
					})
				if err != nil {
					return "", err
				}
				newKey = promoted.Key
				return promoted.Key, nil
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if newLease.LeaseID == oldLease.LeaseID || newKey == "" || newKey == initial.Key {
		t.Fatalf("fresh lease or catalog key missing: %+v, %q", newLease, newKey)
	}
	active, done, err := lifecycle.active.Begin(ctx, owner, newLease)
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := active.(*admittedRead)
	if !ok || bound.reader.catalog != catalog || bound.reader.key != newKey {
		done()
		t.Fatal("fresh reader was not bound to the published catalog key")
	}
	done()
	if _, err := catalog.Resolve(initial.Key); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("old key survived promotion: %v", err)
	}
	if err := lifecycle.Read(ctx, owner, newLease, func(tx *sql.Tx) error {
		var got string
		if err := tx.QueryRow("SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?", draftID).Scan(&got); err != nil {
			return err
		}
		if got != changed {
			return ErrChanged
		}
		return nil
	}); err != nil {
		t.Fatalf("catalog-backed fresh lease read: %v", err)
	}
	if _, done, err := lifecycle.active.Begin(ctx, owner, oldLease); !errors.Is(err, runtime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatalf("old lease survived commit: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(retainedPath, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(retained, oldDatabase) {
		t.Fatalf("old source bytes were not retained: %v", err)
	}
}

func configWithoutProject(config projectaccess.Config) projectaccess.Config {
	config.ProjectPath = ""
	return config
}
