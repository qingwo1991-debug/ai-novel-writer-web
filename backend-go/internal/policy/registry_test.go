package policy

import (
	"os"
	"strings"
	"testing"
)

func TestExistingPublicInventory(t *testing.T) {
	file, err := os.Open("../../../web/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	registry, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[Scope]int{}
	for _, scope := range registry.Operations {
		counts[scope]++
	}
	if counts[Project] != 218 || counts[Global] != 42 || counts[Flexible] != 22 || len(registry.Excluded) != 13 {
		t.Fatal(counts, len(registry.Excluded))
	}
	if _, ok := registry.Lookup("web:library-list"); ok {
		t.Fatal("internal helper exposed")
	}
	if _, ok := registry.Lookup("window:close"); ok {
		t.Fatal("desktop control exposed")
	}
}
func TestInvalidPoliciesFailClosed(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"version":1,"operations":{"web:private":"global"}}`,
		`{"version":1,"operations":{"db:a":"unknown"}}`,
		`{"version":1,"operations":{"db:a":"global"},"excluded":{"db:a":"not public"}}`,
		`{"version":1,"operations":{"db:a":"global"}} {}`,
	} {
		if _, err := Load(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
