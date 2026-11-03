//go:build linux

package authorruntime

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectgrant"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

// OpenGrantedProjectReadOnly consumes a caller-owned, one-use selection of an
// already registered project. It does not register or revoke a catalog key:
// doing so before SQLite admission could disturb a healthy active reader if
// the selected project is subsequently refused.
func (l *ReadLifecycle) OpenGrantedProjectReadOnly(ctx context.Context, owner projectsession.Owner,
	grants *projectgrant.Registry, grantID, requestToken string) (ProjectOpenResult, error) {
	if l == nil || l.active == nil || l.catalog == nil || grants == nil || ctx == nil ||
		grantID == "" || requestToken == "" {
		return ProjectOpenResult{}, ErrInvalid
	}
	project, err := grants.Resolve(ctx, owner, projectgrant.PurposeProjectOpen, grantID)
	if err != nil {
		return ProjectOpenResult{}, err
	}
	key, keyErr := l.catalog.KeyForPinnedProject(project)
	closeErr := project.Close()
	if keyErr != nil || closeErr != nil {
		return ProjectOpenResult{}, errors.Join(keyErr, closeErr)
	}
	return l.OpenProjectReadOnly(ctx, owner, key, requestToken)
}
