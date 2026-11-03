//go:build lance_native && cgo

package projectaccess

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestNativeKnowledgeCaptureBorrowsExactLegacyLock(t *testing.T) {
	config := nativeAccessFixture(t)
	before := nativeSourceManifest(t, config)
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	capture, report, err := InspectKnowledgeUnderLock(context.Background(), config, held)
	if err != nil || capture == nil || report.Knowledge == nil || capture.View() == nil || capture.Search() == nil {
		t.Fatal(report, err)
	}
	if !reflect.DeepEqual(before, nativeSourceManifest(t, config)) {
		t.Fatal("source changed during borrowed capture")
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		t.Fatal("capture closed the borrowed lock", err)
	}
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("second writer entered before release: %v", err)
	}
	if _, _, err := InspectKnowledgeUnderLock(context.Background(), config, nil); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("nil lock accepted: %v", err)
	}
}
