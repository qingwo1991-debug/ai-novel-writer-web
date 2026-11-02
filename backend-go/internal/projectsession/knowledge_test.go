package projectsession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)

func knowledgeSessionView(t *testing.T) *knowledgequery.View {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	view, err := knowledgequery.New(context.Background(), knowledgedomain.Snapshot{}, root, knowledgecopy.Budget{MaxMetadataBytes: 1024, MaxCopies: 10})
	if err != nil {
		t.Fatal(err)
	}
	return view
}
func knowledgeSessionFixture(t *testing.T) (*KnowledgeManager, Owner, Context, *knowledgequery.View) {
	t.Helper()
	m := NewKnowledgeManager()
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	view := knowledgeSessionView(t)
	ctx, err := m.adoptKnowledge(context.Background(), owner, "/synthetic/project", "project-id", view, map[string]int64{"doc": 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m, owner, ctx, view
}
func TestKnowledgeSessionWholeCallLease(t *testing.T) {
	m, owner, frozen, want := knowledgeSessionFixture(t)
	view, chapters, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
	if err != nil || view != want || chapters["doc"] != 7 || done == nil {
		t.Fatal(view, chapters, err)
	}
	if err := m.Release(owner, frozen); !errors.Is(err, novelruntime.ErrBusy) {
		t.Fatal(err)
	}
	done()
	done()
	if err := m.Release(owner, frozen); err != nil {
		t.Fatal(err)
	}
	view, chapters, done, err = m.BeginKnowledge(context.Background(), owner, frozen)
	if !errors.Is(err, novelruntime.ErrNotOwned) || view != nil || chapters != nil || done != nil {
		t.Fatal(view, chapters, err)
	}
}
func TestKnowledgeSessionExactOwnerAndFrozenContext(t *testing.T) {
	m, owner, frozen, _ := knowledgeSessionFixture(t)
	for _, kind := range []string{"session", "tab", "id", "lease", "path", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			who := owner
			expected := frozen
			switch kind {
			case "session":
				who.SessionID = "another-cookie"
			case "tab":
				who.ClientID = "another-tab"
			case "id":
				expected.ProjectID = "another-project"
			case "lease":
				expected.LeaseID = "stale-lease"
			case "path":
				expected.ProjectPath = "/synthetic/other"
			case "unknown":
				expected.ProjectPath = "/does-not-exist/on-server"
			}
			view, chapters, done, err := m.BeginKnowledge(context.Background(), who, expected)
			if !errors.Is(err, novelruntime.ErrNotOwned) || view != nil || chapters != nil || done != nil {
				t.Fatal(view, chapters, err)
			}
			if err := m.Release(who, expected); !errors.Is(err, novelruntime.ErrNotOwned) {
				t.Fatal(err)
			}
		})
	}
	view, _, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
	if err != nil || view == nil {
		t.Fatal(err)
	}
	done()
}
func TestKnowledgeSessionRefreshRetainsFixedCaptureAndCopiesMaps(t *testing.T) {
	m, owner, frozen, want := knowledgeSessionFixture(t)
	input := map[string]int64{"replacement": 99}
	replacement := knowledgeSessionView(t)
	refresh, err := m.adoptKnowledge(context.Background(), owner, frozen.ProjectPath, frozen.ProjectID, replacement, input)
	if err != nil || refresh != frozen {
		t.Fatal(refresh, err)
	}
	view, chapters, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
	if err != nil || view != want || !reflect.DeepEqual(chapters, map[string]int64{"doc": 7}) {
		t.Fatal(chapters, err)
	}
	chapters["doc"] = 8
	chapters["injected"] = 1
	done()
	_, again, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
	if err != nil || !reflect.DeepEqual(again, map[string]int64{"doc": 7}) {
		t.Fatal(again, err)
	}
	done()
	if _, err := m.adoptKnowledge(context.Background(), Owner{SessionID: "cookie", ClientID: "foreign"}, frozen.ProjectPath, frozen.ProjectID, want, nil); !errors.Is(err, novelruntime.ErrOwned) {
		t.Fatal(err)
	}
	if _, err := m.adoptKnowledge(context.Background(), owner, frozen.ProjectPath, "changed-id", want, nil); !errors.Is(err, novelruntime.ErrNotOwned) {
		t.Fatal(err)
	} // Public refresh bypasses inspection and configuration changes: deliberately fixed captured state.
	public, err := m.OpenKnowledge(context.Background(), owner, projectaccess.Config{ProjectPath: frozen.ProjectPath, Knowledge: &projectaccess.KnowledgeConfig{Native: true}})
	if err != nil || public != frozen {
		t.Fatal(public, err)
	}
}
func TestKnowledgeSessionCloseIsAtomicAndRetryable(t *testing.T) {
	m, owner, first, view := knowledgeSessionFixture(t)
	second, err := m.adoptKnowledge(context.Background(), owner, "/synthetic/second", "second-id", view, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, done, err := m.BeginKnowledge(context.Background(), owner, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); !errors.Is(err, novelruntime.ErrBusy) {
		t.Fatal(err)
	}
	_, _, firstDone, err := m.BeginKnowledge(context.Background(), owner, first)
	if err != nil {
		t.Fatal("close partially revoked idle project", err)
	}
	firstDone()
	done()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.BeginKnowledge(context.Background(), owner, first); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := m.Release(owner, first); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := m.adoptKnowledge(context.Background(), owner, first.ProjectPath, first.ProjectID, view, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}
func TestKnowledgeSessionCancellationAndInvalidInputsZeroBorrow(t *testing.T) {
	m, owner, frozen, view := knowledgeSessionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, who := range []Owner{owner, {SessionID: "bad:cookie", ClientID: "tab"}, {SessionID: "cookie", ClientID: ""}} {
		got, chapters, done, err := m.BeginKnowledge(ctx, who, frozen)
		if err == nil || got != nil || chapters != nil || done != nil {
			t.Fatal(got, chapters, err)
		}
	}
	if _, _, _, err := m.BeginKnowledge(nil, owner, frozen); !errors.Is(err, ErrContext) {
		t.Fatal(err)
	}
	if _, err := m.adoptKnowledge(ctx, owner, "/synthetic/canceled", "id", view, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(m.mounts) != 1 {
		t.Fatal("canceled adoption retained mount")
	}
	if err := m.Release(owner, frozen); err != nil {
		t.Fatal("canceled borrow leaked hold", err)
	}
}
func TestKnowledgeSessionConcurrentBorrowsFenceReleaseAndClose(t *testing.T) {
	m, owner, frozen, want := knowledgeSessionFixture(t)
	const count = 24
	ready := make(chan struct{}, count)
	finish := make(chan struct{})
	failures := make(chan error, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			view, chapters, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
			if err != nil {
				failures <- err
				ready <- struct{}{}
				return
			}
			if view != want || chapters["doc"] != 7 {
				failures <- ErrContext
			}
			chapters["doc"] = 123
			ready <- struct{}{}
			<-finish
			done()
			done()
		}()
	}
	for range count {
		<-ready
	}
	releaseErr := m.Release(owner, frozen)
	closeErr := m.Close()
	close(finish)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if !errors.Is(releaseErr, novelruntime.ErrBusy) || !errors.Is(closeErr, novelruntime.ErrBusy) {
		t.Fatal(releaseErr, closeErr)
	}
	if err := m.Release(owner, frozen); err != nil {
		t.Fatal(err)
	}
}
func TestKnowledgeSessionPublicInspectFailureNeverReservesLease(t *testing.T) {
	m := NewKnowledgeManager()
	defer m.Close()
	base := t.TempDir()
	data := filepath.Join(base, "missing-data")
	config := projectaccess.Config{DataRoot: data, LibraryRoot: filepath.Join(data, "projects"), ProjectPath: filepath.Join(data, "projects/synthetic"), ScratchParent: filepath.Join(base, "missing-scratch"), MaxDatabaseBytes: 1024, Knowledge: &projectaccess.KnowledgeConfig{Native: true, Copy: projectfs.KnowledgeBudget{MaxBytes: 1024, MaxEntries: 10, MaxDepth: 4}, Metadata: knowledgecopy.Budget{MaxMetadataBytes: 1024, MaxCopies: 10}}}
	got, err := m.OpenKnowledge(context.Background(), Owner{SessionID: "cookie", ClientID: "tab"}, config)
	if err == nil || got != (Context{}) || len(m.mounts) != 0 {
		t.Fatal(got, err)
	}
	if _, err := os.Lstat(data); !os.IsNotExist(err) {
		t.Fatal("failed inspection created author data", err)
	}
	if _, err := os.Lstat(config.ScratchParent); !os.IsNotExist(err) {
		t.Fatal("failed inspection created scratch", err)
	}
	config.Knowledge = nil
	if _, err := m.OpenKnowledge(context.Background(), Owner{SessionID: "cookie", ClientID: "tab"}, config); !errors.Is(err, projectaccess.ErrConfiguration) {
		t.Fatal(err)
	}
}
func TestKnowledgeSessionPrivateAdoptionBoundsAndMapOwnership(t *testing.T) {
	view := knowledgeSessionView(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	for _, kind := range []string{"relative", "unclean", "root", "empty-id", "invalid-id", "nil-view", "empty-finalized-id", "unsafe-chapter", "invalid-utf8"} {
		t.Run(kind, func(t *testing.T) {
			m := NewKnowledgeManager()
			defer m.Close()
			path, id, borrowed := "/synthetic/project", "id", view
			finalized := map[string]int64{"doc": 1}
			switch kind {
			case "relative":
				path = "relative/project"
			case "unclean":
				path = "/synthetic/../project"
			case "root":
				path = "/"
			case "empty-id":
				id = ""
			case "invalid-id":
				id = string([]byte{255})
			case "nil-view":
				borrowed = nil
			case "empty-finalized-id":
				finalized = map[string]int64{"": 1}
			case "unsafe-chapter":
				finalized["doc"] = 9007199254740992
			case "invalid-utf8":
				finalized = map[string]int64{string([]byte{255}): 1}
			}
			got, err := m.adoptKnowledge(context.Background(), owner, path, id, borrowed, finalized)
			if !errors.Is(err, ErrContext) || got != (Context{}) || len(m.mounts) != 0 {
				t.Fatal(got, err)
			}
		})
	}
	m := NewKnowledgeManager()
	defer m.Close()
	input := map[string]int64{"doc": 2}
	frozen, err := m.adoptKnowledge(context.Background(), owner, "/synthetic/valid", "id", view, input)
	if err != nil {
		t.Fatal(err)
	}
	input["doc"] = 999
	_, actual, done, err := m.BeginKnowledge(context.Background(), owner, frozen)
	if err != nil || actual["doc"] != 2 {
		t.Fatal(actual, err)
	}
	done()
}
