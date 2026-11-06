//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// productionResidualSourceLine reads one line of one file, trimmed. Both residual
// records cite file:line pairs, so both tests read them through this one function
// and fail when a cited declaration has moved. It is deliberately a plain read
// rather than an AST walk: the markers are declaration prefixes and the point is
// that the file still says what the record claims, in the place it claims it.
func productionResidualSourceLine(path string, line int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	if line < 1 || line > len(lines) {
		return "", &productionResidualLineError{Path: path, Line: line, Total: len(lines)}
	}
	return strings.TrimSpace(lines[line-1]), nil
}

// productionResidualLineError says which file ran out of lines, without quoting any
// content: the path and two counts are all a reader needs to find the moved line.
type productionResidualLineError struct {
	Path  string
	Line  int
	Total int
}

func (e *productionResidualLineError) Error() string {
	return e.Path + " has " + strconv.Itoa(e.Total) + " lines, so it cannot carry line " +
		strconv.Itoa(e.Line)
}

// productionResidualSeams walks a seam list: every port must be a declaration that
// still sits on the cited line, and every seam must name its own family, at least
// one port and the sentence that says what lifting the refusal would take.
func productionResidualSeams(t *testing.T, family string, seams []productionInlineResidualSeam) {
	t.Helper()
	if len(seams) == 0 {
		t.Fatalf("%s records no refused names", family)
	}
	for _, seam := range seams {
		if seam.Operation == "" || seam.Family != family {
			t.Fatalf("%s records a seam for %v in family %v", family, seam.Operation, seam.Family)
		}
		if strings.TrimSpace(seam.Answer) == "" {
			t.Fatalf("%s says nothing about lifting %v", family, seam.Operation)
		}
		if len(seam.Missing) == 0 {
			t.Fatalf("%s cites no missing port for %v", family, seam.Operation)
		}
		for _, port := range seam.Missing {
			if port.Path == "" || port.Line < 1 || strings.TrimSpace(port.Marker) == "" ||
				strings.TrimSpace(port.Because) == "" {
				t.Fatalf("%s cites an incomplete port for %v", family, seam.Operation)
			}
			found, err := productionResidualSourceLine(port.Path, port.Line)
			if err != nil {
				t.Fatalf("%s cites %v: %v", family, port.Path, err)
			}
			if !strings.Contains(found, port.Marker) {
				t.Fatalf("%s cites %v line %v for %v, but that line no longer carries it",
					family, port.Path, port.Line, port.Marker)
			}
		}
	}
}

// productionResidualInventoried reads the browser's inventory and requires every
// name in this record to be listed there for a project-scoped operation. The
// inventory is the authority for the spelling of a name, so a typo in the record
// fails here instead of surviving as a seam for an operation nobody can call.
func productionResidualInventoried(t *testing.T, names []string) {
	t.Helper()
	raw, err := os.ReadFile("../../../web/operations.json")
	if err != nil {
		t.Fatalf("%v", err)
	}
	inventory := string(raw)
	for _, name := range names {
		entry := "\"" + name + "\": \"project\""
		if !strings.Contains(inventory, entry) {
			t.Fatalf("web/operations.json does not list %v as a project operation", name)
		}
	}
}

// TestProductionResidualRecords proves both residual records are still true: every
// cited declaration is where the record says it is, and every recorded name is one
// the browser can actually ask for.
func TestProductionResidualRecords(t *testing.T) {
	editor := productionInlineResidualEditorSeams()
	review := productionInlineResidualReviewSeams()
	productionResidualSeams(t, "editor-inline", editor)
	productionResidualSeams(t, "review-revision", review)
	// Each family is four names in the inventory; the record has to account for all
	// of them, the served half and the refused half together.
	for _, family := range []struct {
		label   string
		served  []string
		refused []productionInlineResidualSeam
	}{
		{"editor-inline", productionInlineResidualEditorServed(), editor},
		{"review-revision", productionInlineResidualReviewServed(), review},
	} {
		if len(family.served)+len(family.refused) != 4 {
			t.Fatalf("%s accounts for %v of its four names", family.label,
				len(family.served)+len(family.refused))
		}
		names := append([]string{}, family.served...)
		for _, seam := range family.refused {
			names = append(names, seam.Operation)
		}
		seen := map[string]bool{}
		for _, name := range names {
			if seen[name] {
				t.Fatalf("%s records %v twice", family.label, name)
			}
			seen[name] = true
		}
		productionResidualInventoried(t, names)
	}
}

// TestProductionResidualBoundNames proves the record matches the built lane: every
// name the records call served is bound, every name they call refused is not bound
// at all, so it still fails closed at GO_OPERATION_NOT_MIGRATED rather than
// answering a refusal of its own. The fixture is the editor-read root and it skips
// without GO_SQLITE_CONTRACT exactly like its siblings.
func TestProductionResidualBoundNames(t *testing.T) {
	root, _, contract, _, _ := productionEditorReadsRoot(t)
	registry := migrationRegistry(t)
	handler, cleanup, err := productionHandlerWithContract(registry, productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	bound := map[string]bool{}
	for _, name := range productionLaneBoundOperations(t, handler) {
		bound[name] = true
	}
	for _, name := range productionInlineResidualEditorServed() {
		if !bound[name] {
			t.Fatalf("editor-inline serves its read path, but %v is not bound", name)
		}
	}
	for _, name := range productionInlineResidualReviewServed() {
		if !bound[name] {
			t.Fatalf("review-revision serves its write path, but %v is not bound", name)
		}
	}
	refused := append([]productionInlineResidualSeam{}, productionInlineResidualEditorSeams()...)
	refused = append(refused, productionInlineResidualReviewSeams()...)
	for _, seam := range refused {
		if bound[seam.Operation] {
			t.Fatalf("%v is bound, but this record says it cannot answer yet", seam.Operation)
		}
	}
}
