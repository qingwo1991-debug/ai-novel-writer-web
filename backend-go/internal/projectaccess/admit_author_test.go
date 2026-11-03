//go:build linux

package projectaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func admittedAuthorFixture(t *testing.T) (Config, *projectfs.Project, *writerlock.Lock) {
	t.Helper()
	config := accessFixture(t)
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Close() })
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { library.Close() })
	project, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { project.Close() })
	return config, project, held
}

func TestAdmitAuthorAcceptsCleanBaseline(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	permit, err := AdmitAuthor(context.Background(), config, project, held)
	if err != nil {
		t.Fatalf("clean baseline not admitted: %v", err)
	}
	defer permit.Close()
	if err := permit.Verify(context.Background(), project); err != nil {
		t.Fatalf("verify failed on unchanged project: %v", err)
	}
	if err := permit.Verify(context.Background(), project); err != nil {
		t.Fatalf("verify is not re-usable: %v", err)
	}
}

func TestAdmitAuthorFailsOnWrongProject(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	otherConfig := config
	otherConfig.ProjectPath = "different-project"
	if _, err := AdmitAuthor(context.Background(), otherConfig, project, held); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("mismatched project admitted: %v", err)
	}
	if _, err := AdmitAuthor(context.Background(), config, nil, held); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("nil project admitted: %v", err)
	}
}

func TestAdmitAuthorRequiresHeldLock(t *testing.T) {
	config, project, _ := admittedAuthorFixture(t)
	if _, err := AdmitAuthor(context.Background(), config, project, nil); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("nil lock admitted: %v", err)
	}
}

func TestAdmitAuthorVerifyDetectsReplacedDatabase(t *testing.T) {
	config, project, held := admittedAuthorFixture(t)
	permit, err := AdmitAuthor(context.Background(), config, project, held)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	if err := permit.Verify(context.Background(), project); err != nil {
		t.Fatalf("pre-change verify failed: %v", err)
	}
	// Replacing the database entry breaks the content identity proof.
	writeAccess(t, configProjectDB(t, config), []byte("replacement database"))
	if err := permit.Verify(context.Background(), project); !errors.Is(err, ErrAuthorStale) {
		t.Fatalf("replaced database verified: %v", err)
	}
	if err := permit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := permit.Verify(context.Background(), project); !errors.Is(err, projectfs.ErrClosed) {
		t.Fatalf("closed permit still verifies: %v", err)
	}
}

func configProjectDB(t *testing.T, config Config) string {
	t.Helper()
	return config.ProjectPath + "/.ai-novel/project.db"
}