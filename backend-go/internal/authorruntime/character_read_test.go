//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func TestReadLifecycleCharactersKeepsExactLeaseAndSource(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "character-cookie", ClientID: "character-tab"}
	other := projectsession.Owner{SessionID: "character-cookie", ClientID: "other-tab"}
	identity, err := lifecycle.Open(context.Background(), owner, key, "character-open")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := lifecycle.ReadCharactersJSON(context.Background(), owner, identity, "db:character-get-all")
	if err != nil {
		t.Fatal(err)
	}
	var rows []any
	if err := json.Unmarshal(wire, &rows); err != nil || rows == nil {
		t.Fatalf("character read was not a frozen JSON array: %s %v", wire, err)
	}
	if _, err := lifecycle.ReadCharactersJSON(context.Background(), other, identity, "db:character-get-all"); err == nil {
		t.Fatal("cross-tab character read accepted")
	}
	stale := identity
	stale.LeaseID = "different-lease"
	if _, err := lifecycle.ReadCharactersJSON(context.Background(), owner, stale, "db:character-get-all"); err == nil {
		t.Fatal("stale character lease accepted")
	}
	if _, err := lifecycle.ReadCharactersJSON(context.Background(), owner, identity, "db:character-roster-commit"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("character write operation accepted: %v", err)
	}
	if err := lifecycle.Close(context.Background(), owner, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ReadCharactersJSON(context.Background(), owner, identity, "db:character-get-all"); err == nil {
		t.Fatal("closed project character read accepted")
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("character read changed author database: %v", err)
	}
}

func TestPrivateCharacterReadRejectsSubstitutedSnapshotPath(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	path := filepath.Join(reader.directory, "snapshot.db")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, path); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readCharactersJSON(context.Background(), reader.report.ProjectID, "db:character-get-all"); !errors.Is(err, ErrChanged) {
		t.Fatalf("substituted private snapshot accepted: %v", err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("substituted snapshot touched source: %v", err)
	}
}
