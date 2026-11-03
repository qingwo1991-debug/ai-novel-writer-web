//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func TestKnowledgeReadRequiresNativeConfigurationWithoutChangingProjectLease(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	expected, err := lifecycle.Open(context.Background(), owner, key, "open")
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := lifecycle.BeginKnowledgeRead(context.Background(), owner, expected)
	if !errors.Is(err, projectaccess.ErrConfiguration) || borrow.Release != nil || borrow.View != nil || borrow.Search != nil {
		t.Fatalf("native-disabled knowledge was admitted: %+v %v", borrow, err)
	}
	current, present, err := lifecycle.Current(context.Background(), owner)
	if err != nil || !present || current != expected {
		t.Fatalf("failed knowledge borrow changed project lease: %+v %v", current, err)
	}
}
