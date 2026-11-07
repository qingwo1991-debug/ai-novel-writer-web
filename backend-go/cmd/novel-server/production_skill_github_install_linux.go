//go:build linux

package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/skillegress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the serving half of the two GitHub skill names the sibling seam
// (production_skill_github_linux.go) left refused:
//
//	skills:inspect-github   args [sourceUrl]  -> {success, inspection?}
//	skills:install-github   args [sourceUrl]  -> {success, skill?}
//
// The sibling's refusal was a POLICY conclusion: the lane's only admitted
// outbound client was the model egress (internal/egress), whose origin set is
// the author's committed model allowlist and therefore never admits GitHub.
// That conclusion is correct as far as the model policy goes, and it is not the
// only technical answer: this file builds a second, narrow egress whose origin
// set is fixed at exactly two GitHub origins (internal/skillegress), and serves
// both names through the two ports promptskills already implements
// (promptskills.InspectGitHub and promptskills.InstallGitHub, remote.go:40/:47).
//
// # The narrow outbound path
//
// internal/skillegress is a purpose-built client, not a second general HTTP
// stack: its origin set is two package constants (https://api.github.com and
// https://raw.githubusercontent.com), no argument, header, committed value or
// environment variable reaches its constructor, it refuses a downgrade, a port,
// a credential, a query/fragment, any 3xx, an oversize body and a slow fetch,
// and it never follows a redirect. Every user-supplied source URL is parsed by
// promptskills.ParseGitHubURL before any socket, and the two endpoints are
// reconstructed from that parse's safe parts (remote.go:34-36), so a foreign
// host is refused before the narrow client is ever asked to dial.
//
// # The dispatcher and the authority
//
// The dispatcher is the narrow client itself: *skillegress.Client satisfies
// promptskills.GitHubDispatcher, pinned by the compile-time assertion below.
// The authority is productionSkillGitHubAuthority: on every admission it
// re-verifies the lane's pinned generation identity through
// globaladmission.PinnedReady.VerifyIdentity and refuses a caller whose
// generation is not the one still occupying the committed paths. The lane pins
// one generation for the process lifetime and has no rollover, so the release
// is a no-op; the re-verification is the strongest "still current" proof this
// lane can make, and the write is atomic underneath it.
//
// # One shared service
//
// InspectGitHub mints a single-use receipt keyed by caller plus source URL, and
// InstallGitHub spends it (remote.go:16, :47-49). The two handlers therefore
// share ONE promptskills.Service over the committed generation's data root, the
// same root the local install and the prompt reads use
// (productionPromptWrites.pinnedRoot). The service is built once at Bind time,
// because a fresh per-request service would drop the receipt between inspect
// and install.
//
// # Wire shape
//
// The desktop answers both names with HTTP 200 and a {success, error} envelope
// (electron/controllers/app-data-controller.ts:440-485); src/shared/ipc-channels.ts
// types the return the same way. Success carries the inspection or the installed
// skill; a refusal carries a fixed symbolic code, never a host, a path, a URL or
// an os sentence. The fetched bytes are judged and published only by
// promptskills itself, so a traversal or an incompatible hook is refused by the
// same inspect core the local install applies, and the skill root stays
// byte-identical after every refusal.

// The narrow client is the dispatcher the promptskills remote ports take. This
// assertion fails the build the day the two signatures drift apart.
var _ promptskills.GitHubDispatcher = (*skillegress.Client)(nil)

// The refusal vocabulary. Every code is symbolic and fixed: no host, no path,
// no committed value and no os sentence is ever formatted into one.
const (
	// productionSkillGitHubInstallLaneCode: a nil lane, a nil server, or a bind
	// after the lane was closed.
	productionSkillGitHubInstallLaneCode = "SKILLS_GITHUB_LANE_UNAVAILABLE"
	// productionSkillGitHubInstallRootCode: the committed generation root could
	// not be re-proved, or the shared service could not be opened or authorised.
	productionSkillGitHubInstallRootCode = "SKILLS_GITHUB_ROOT_UNAVAILABLE"
	// productionSkillGitHubInstallFetchCode: the narrow client could not be built.
	productionSkillGitHubInstallFetchCode = "SKILLS_GITHUB_FETCH_UNAVAILABLE"
)

// productionSkillGitHubAuthority is the RemoteAuthority the shared service
// admits by. It lends a read lease on the one generation this lane pinned, by
// re-proving that the pinned receipt and generation still occupy the same paths
// at the moment of admission.
type productionSkillGitHubAuthority struct {
	open *productionOpen
}

// AcquireCurrent verifies the caller's generation against the lane's current
// committed generation and returns a release. The release is a no-op because
// this lane has no generation rollover and pins one generation for the process
// lifetime; the re-verification below is what makes a stale generation refuse.
func (a *productionSkillGitHubAuthority) AcquireCurrent(ctx context.Context, caller promptskills.Caller) (func(), error) {
	if a == nil || a.open == nil {
		return nil, promptskills.ErrRemoteUnavailable
	}
	if caller.Principal == "" || caller.SessionID == "" || caller.ClientID == "" || caller.Generation == "" {
		return nil, promptskills.ErrInspectFirst
	}
	if ctx == nil {
		return nil, promptskills.ErrRemoteUnavailable
	}
	open := a.open
	open.mu.Lock()
	global, closed := open.global, open.closed
	open.mu.Unlock()
	if closed || global == nil {
		return nil, promptskills.ErrRemoteUnavailable
	}
	ready, err := global.VerifyIdentity(ctx)
	if err != nil {
		return nil, promptskills.ErrRemoteUnavailable
	}
	if ready.GlobalGeneration == "" || caller.Generation != ready.GlobalGeneration {
		return nil, promptskills.ErrRemoteUnavailable
	}
	return func() {}, nil
}

// productionSkillGitHubInstall is the serving binder for the two names. It owns
// one narrow client, one shared promptskills.Service over the committed
// generation, and the generation string the caller is scoped by. The client
// field is nil until Bind builds it, and is injectable only so a test can stub
// the socket through skillegress.NewWithTransport - never from an operation
// argument.
type productionSkillGitHubInstall struct {
	open       *productionOpen
	client     *skillegress.Client
	mu         sync.Mutex
	service    *promptskills.Service
	generation string
	closed     bool
}

// newProductionSkillGitHubInstall returns the serving binder for the lane built
// by productionHandlerWithContract. A nil lane returns nil.
func newProductionSkillGitHubInstall(open *productionOpen) *productionSkillGitHubInstall {
	return newProductionSkillGitHubInstallWithClient(open, nil)
}

// newProductionSkillGitHubInstallWithClient is the test seam: a pre-built narrow
// client (over a stubbed socket) is used instead of dialing the real network.
func newProductionSkillGitHubInstallWithClient(open *productionOpen, client *skillegress.Client) *productionSkillGitHubInstall {
	return &productionSkillGitHubInstall{open: open, client: client}
}

// Bind builds the narrow client and the shared service, then installs both
// handlers. It fails loudly on any missing port rather than installing a
// handler that could only refuse, and it binds nothing unless every port is
// ready.
func (g *productionSkillGitHubInstall) Bind(server *transport.Server) error {
	if g == nil || g.open == nil || server == nil {
		return errors.New(productionSkillGitHubInstallLaneCode)
	}
	client := g.client
	if client == nil {
		var err error
		client, err = skillegress.New()
		if err != nil {
			return errors.New(productionSkillGitHubInstallFetchCode)
		}
	}
	open := g.open
	open.mu.Lock()
	global, closed := open.global, open.closed
	open.mu.Unlock()
	if closed || global == nil {
		return errors.New(productionSkillGitHubInstallRootCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), productionPromptRootTimeout)
	defer cancel()
	ready, err := global.VerifyIdentity(ctx)
	if err != nil {
		return errors.New(productionSkillGitHubInstallRootCode)
	}
	if ready.DataRoot == "" || ready.GlobalGeneration == "" {
		return errors.New(productionSkillGitHubInstallRootCode)
	}
	service, err := promptskills.New(ready.DataRoot)
	if err != nil {
		return errors.New(productionSkillGitHubInstallRootCode)
	}
	if err := service.SetRemoteAuthority(&productionSkillGitHubAuthority{open: g.open}); err != nil {
		_ = service.Close()
		return errors.New(productionSkillGitHubInstallRootCode)
	}
	if err := server.Bind(productionSkillGitHubInspectName, g.inspect); err != nil {
		_ = service.Close()
		return err
	}
	if err := server.Bind(productionSkillGitHubInstallName, g.install); err != nil {
		_ = service.Close()
		return err
	}
	g.mu.Lock()
	g.service = service
	g.client = client
	g.generation = ready.GlobalGeneration
	g.mu.Unlock()
	return nil
}

// Close releases the shared service and drops the client. It is idempotent and
// safe to call after the handlers are no longer reachable.
func (g *productionSkillGitHubInstall) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	service := g.service
	g.service = nil
	g.client = nil
	g.generation = ""
	closed := g.closed
	g.closed = true
	g.mu.Unlock()
	if !closed && service != nil {
		_ = service.Close()
	}
}

// inspect is the skills:inspect-github handler. It returns the inspection for a
// compatible or incompatible package alike; only a refused source, redirect,
// oversize body or failed fetch is an error.
func (g *productionSkillGitHubInstall) inspect(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	source, ok := productionSkillGitHubSourceArg(args)
	if !ok {
		return productionSkillGitHubFailure("INVALID_ARGUMENTS"), nil
	}
	if ctx == nil {
		return productionSkillGitHubFailure("REQUEST_CANCELED"), nil
	}
	service, pc, client, code := g.prepare(caller)
	if service == nil {
		return productionSkillGitHubFailure(code), nil
	}
	inspection, err := service.InspectGitHub(ctx, pc, source, client)
	if err != nil {
		return productionSkillGitHubFailure(productionSkillGitHubInstallError(err)), nil
	}
	return map[string]any{"success": true, "inspection": inspection}, nil
}

// install is the skills:install-github handler. promptskills refetches the bytes,
// compares the digest and resolved URL against the inspection receipt, and
// publishes only those re-verified bytes through the same inspect gate the local
// install applies.
func (g *productionSkillGitHubInstall) install(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	source, ok := productionSkillGitHubSourceArg(args)
	if !ok {
		return productionSkillGitHubFailure("INVALID_ARGUMENTS"), nil
	}
	if ctx == nil {
		return productionSkillGitHubFailure("REQUEST_CANCELED"), nil
	}
	service, pc, client, code := g.prepare(caller)
	if service == nil {
		return productionSkillGitHubFailure(code), nil
	}
	skill, err := service.InstallGitHub(ctx, pc, source, client)
	if err != nil {
		return productionSkillGitHubFailure(productionSkillGitHubInstallError(err)), nil
	}
	return map[string]any{"success": true, "skill": skill}, nil
}

// prepare snapshots the shared service, the pinned generation and the narrow
// client under the lock and builds the promptskills caller. An unidentified
// session tab is refused before any service is borrowed or any byte is fetched.
func (g *productionSkillGitHubInstall) prepare(caller transport.Caller) (*promptskills.Service, promptskills.Caller, *skillegress.Client, string) {
	if g == nil || productionGlobalCaller(caller) == "" {
		return nil, promptskills.Caller{}, nil, "GLOBAL_DATA_NOT_READY"
	}
	g.mu.Lock()
	service, generation, client, closed := g.service, g.generation, g.client, g.closed
	g.mu.Unlock()
	if closed || service == nil || client == nil || generation == "" {
		return nil, promptskills.Caller{}, nil, productionSkillGitHubInstallRootCode
	}
	pc := promptskills.Caller{
		Principal:  caller.Principal,
		SessionID:  caller.SessionID,
		ClientID:   caller.ClientID,
		Generation: generation,
	}
	return service, pc, client, ""
}

// productionSkillGitHubSourceArg decodes the one string argument both names
// take. A missing, extra or non-string argument is a shape refusal.
func productionSkillGitHubSourceArg(args []any) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	source, ok := args[0].(string)
	if !ok {
		return "", false
	}
	return source, true
}

// productionSkillGitHubFailure is the wire envelope the desktop returns: HTTP 200
// with success false and one fixed error string, never a host or a path.
func productionSkillGitHubFailure(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}

// productionSkillGitHubInstallError maps every refusal to one fixed code. The
// promptskills sentinels are fixed symbols and are safe to surface; every other
// error - including an os write error that may carry a host path, and the
// promptskills prose verdicts - collapses to one refusal so nothing reaches the
// browser verbatim.
func productionSkillGitHubInstallError(err error) string {
	if err == nil {
		return "SKILLS_GITHUB_REFUSED"
	}
	switch {
	case errors.Is(err, promptskills.ErrInspectFirst):
		return err.Error()
	case errors.Is(err, context.Canceled):
		return "REQUEST_CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "REQUEST_DEADLINE_EXCEEDED"
	case errors.Is(err, skillegress.ErrRedirect):
		return "SKILLS_GITHUB_REDIRECT_FORBIDDEN"
	case errors.Is(err, skillegress.ErrNotAllowed):
		return "SKILLS_GITHUB_NOT_ALLOWED"
	case errors.Is(err, skillegress.ErrTooLarge):
		return "SKILLS_GITHUB_TOO_LARGE"
	case errors.Is(err, skillegress.ErrFetchFailed):
		return "SKILLS_GITHUB_FETCH_FAILED"
	case errors.Is(err, promptskills.ErrRemoteUnavailable):
		return "SKILLS_GITHUB_UNAVAILABLE"
	case errors.Is(err, fs.ErrExist):
		return "SKILLS_GITHUB_ALREADY_INSTALLED"
	}
	return "SKILLS_GITHUB_REFUSED"
}

// bindProductionSkillGitHubInstall is the one call the parent adds beside the
// sibling binders. It is the whole wiring for the two names: both are served,
// and a lane that cannot build the narrow client or re-prove its root refuses at
// startup instead of installing a handler that could only refuse.
func bindProductionSkillGitHubInstall(server *transport.Server, lane *productionSkillGitHubInstall) error {
	if lane == nil {
		return errors.New(productionSkillGitHubInstallLaneCode)
	}
	if server == nil {
		return errors.New(productionSkillGitHubInstallLaneCode)
	}
	if err := lane.Bind(server); err != nil {
		return err
	}
	log.Printf("novel-server: %s and %s served over the narrow skill egress",
		productionSkillGitHubInspectName, productionSkillGitHubInstallName)
	return nil
}
