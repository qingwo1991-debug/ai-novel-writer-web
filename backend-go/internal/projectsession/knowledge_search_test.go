package projectsession

import (
	"context"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)

func TestKnowledgeSearchBorrowUsesExactLeaseAndHoldsRelease(t *testing.T) {
	m, owner, frozen, _ := knowledgeSessionFixture(t)
	capture := &knowledgequery.SearchCapture{}
	m.mu.Lock()
	m.mounts[frozen.ProjectPath].search = capture
	m.mu.Unlock()
	for _, variant := range []struct {
		name string
		who  Owner
		ctx  Context
	}{{"cookie", Owner{SessionID: "foreign", ClientID: owner.ClientID}, frozen}, {"tab", Owner{SessionID: owner.SessionID, ClientID: "foreign"}, frozen}, {"lease", owner, Context{ProjectID: frozen.ProjectID, LeaseID: "stale", ProjectPath: frozen.ProjectPath}}, {"project", owner, Context{ProjectID: "other", LeaseID: frozen.LeaseID, ProjectPath: frozen.ProjectPath}}} {
		t.Run(variant.name, func(t *testing.T) {
			got, done, err := m.BeginKnowledgeSearch(context.Background(), variant.who, variant.ctx)
			if !errors.Is(err, novelruntime.ErrNotOwned) || got != nil || done != nil {
				t.Fatal(got, err)
			}
		})
	}
	got, done, err := m.BeginKnowledgeSearch(context.Background(), owner, frozen)
	if err != nil || got != capture || done == nil {
		t.Fatal(got, err)
	}
	if err := m.Release(owner, frozen); !errors.Is(err, novelruntime.ErrBusy) {
		t.Fatal("search did not hold session", err)
	}
	done()
	if err := m.Release(owner, frozen); err != nil {
		t.Fatal(err)
	}
	got, done, err = m.BeginKnowledgeSearch(context.Background(), owner, frozen)
	if !errors.Is(err, novelruntime.ErrNotOwned) || got != nil || done != nil {
		t.Fatal(got, err)
	}
}

func TestKnowledgeSearchUnavailableDoesNotLeakBorrow(t *testing.T) {
	m, owner, frozen, _ := knowledgeSessionFixture(t)
	search, done, err := m.BeginKnowledgeSearch(context.Background(), owner, frozen)
	if !errors.Is(err, ErrContext) || search != nil || done != nil {
		t.Fatal(search, err)
	}
	if err := m.Release(owner, frozen); err != nil {
		t.Fatal("missing search leaked borrow", err)
	}
}
