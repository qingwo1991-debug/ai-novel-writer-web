//go:build linux

package authorruntime

import (
	"context"
	"reflect"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

// KnowledgeBorrow holds the same exact author read lease as project:open. The
// view and search capture are immutable projections of one native-admitted
// private snapshot. Later source changes are not reflected by this capture;
// it is not a claim of current author knowledge state. Release must be called
// after the whole response is built.
type KnowledgeBorrow struct {
	View      *knowledgequery.View
	Search    *knowledgequery.SearchCapture
	Finalized map[string]int64
	Release   func()
}

// BeginKnowledgeRead lazily captures native knowledge under the current
// project's continuously held legacy lock. It never creates a second session
// or a second lease. A failed capture leaves the existing project session
// intact and grants no cached result.
func (l *ReadLifecycle) BeginKnowledgeRead(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (KnowledgeBorrow, error) {
	if l == nil || l.active == nil || l.config.Knowledge == nil || !l.config.Knowledge.Native {
		return KnowledgeBorrow{}, projectaccess.ErrConfiguration
	}
	handle, release, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return KnowledgeBorrow{}, err
	}
	committed := false
	defer func() {
		if !committed {
			release()
		}
	}()
	admitted, ok := handle.(*admittedRead)
	if !ok || admitted.context != expected || admitted.reader == nil {
		return KnowledgeBorrow{}, ErrInvalid
	}
	admitted.knowledgeMu.Lock()
	defer admitted.knowledgeMu.Unlock()
	if err := admitted.reader.Verify(ctx); err != nil {
		return KnowledgeBorrow{}, err
	}
	if admitted.knowledge == nil {
		config := l.config
		config.ProjectPath = expected.ProjectPath
		capture, report, err := projectaccess.InspectKnowledgeUnderLock(ctx, config, admitted.reader.lock)
		if err != nil {
			return KnowledgeBorrow{}, err
		}
		before, err := admitted.reader.Report()
		if err != nil {
			return KnowledgeBorrow{}, err
		}
		if report.ProjectID != expected.ProjectID || !reflect.DeepEqual(report, before) {
			return KnowledgeBorrow{}, ErrChanged
		}
		if err := admitted.reader.Verify(ctx); err != nil {
			return KnowledgeBorrow{}, err
		}
		admitted.knowledge = capture
	}
	if admitted.knowledge.View() == nil || admitted.knowledge.Search() == nil {
		return KnowledgeBorrow{}, ErrInvalid
	}
	committed = true
	return KnowledgeBorrow{
		View: admitted.knowledge.View(), Search: admitted.knowledge.Search(),
		Finalized: admitted.knowledge.FinalizedChapters(), Release: release,
	}, nil
}
