// Package projectgrant issues short lived, one use project open grants from a
// trusted Library. A grant selects a physical project; it does not admit its
// database, publish a writer, or create a browser endpoint.
package projectgrant

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

const PurposeProjectOpen = "project-open"
const MaxTTL = 5 * time.Minute
const maxOutstanding = 1024

var ErrInvalidGrant = errors.New("PROJECT_OPEN_GRANT_INVALID")

type grant struct {
	owner     projectsession.Owner
	purpose   string
	path      string
	projectID string
	identity  projectfs.Identity
	expires   time.Time
}

type Registry struct {
	mu      sync.Mutex
	library *projectfs.Library
	grants  map[string]grant
	now     func() time.Time
}

func New(library *projectfs.Library) (*Registry, error) {
	if library == nil {
		return nil, ErrInvalidGrant
	}
	return &Registry{library: library, grants: make(map[string]grant), now: time.Now}, nil
}

func validOwner(owner projectsession.Owner) bool {
	return owner.SessionID != "" && owner.ClientID != "" &&
		!strings.ContainsRune(owner.SessionID, 0) && !strings.ContainsRune(owner.ClientID, 0)
}

func (r *Registry) pruneLocked(now time.Time) {
	for token, item := range r.grants {
		if !now.Before(item.expires) {
			delete(r.grants, token)
		}
	}
}

// Issue accepts a server-held Project, never a request path. It reopens that
// project through this registry's trusted Library to verify canonical
// membership and physical identity before issuing an opaque grant.
func (r *Registry) Issue(ctx context.Context, owner projectsession.Owner, purpose string,
	project *projectfs.Project, ttl time.Duration) (string, error) {
	if r == nil || r.library == nil || ctx == nil || !validOwner(owner) ||
		purpose != PurposeProjectOpen || project == nil || ttl <= 0 || ttl > MaxTTL {
		return "", ErrInvalidGrant
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	current, err := r.library.OpenProject(project.Path())
	if err != nil {
		return "", ErrInvalidGrant
	}
	if current.ProjectID() != project.ProjectID() || !current.Identity().Same(project.Identity()) {
		_ = current.Close()
		return "", ErrInvalidGrant
	}
	if err := current.Close(); err != nil {
		return "", ErrInvalidGrant
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(nonce[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLocked(now)
	if len(r.grants) >= maxOutstanding {
		return "", ErrInvalidGrant
	}
	if _, exists := r.grants[token]; exists {
		return "", ErrInvalidGrant
	}
	r.grants[token] = grant{owner: owner, purpose: purpose, path: project.Path(),
		projectID: project.ProjectID(), identity: project.Identity(), expires: now.Add(ttl)}
	return token, nil
}

// Resolve consumes a matching grant before touching the filesystem. A foreign
// owner or purpose cannot burn another caller's grant. The returned Project is
// pinned but still requires separate source/SQLite admission; caller closes it.
func (r *Registry) Resolve(ctx context.Context, owner projectsession.Owner,
	purpose, token string) (*projectfs.Project, error) {
	if r == nil || r.library == nil || ctx == nil || !validOwner(owner) ||
		purpose != PurposeProjectOpen || token == "" {
		return nil, ErrInvalidGrant
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	now := r.now()
	r.pruneLocked(now)
	item, ok := r.grants[token]
	if !ok || item.owner != owner || item.purpose != purpose {
		r.mu.Unlock()
		return nil, ErrInvalidGrant
	}
	delete(r.grants, token)
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	project, err := r.library.OpenProject(item.path)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	if project.ProjectID() != item.projectID || !project.Identity().Same(item.identity) {
		_ = project.Close()
		return nil, ErrInvalidGrant
	}
	return project, nil
}
