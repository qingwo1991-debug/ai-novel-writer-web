//go:build linux

package main

import (
	"errors"
	"log"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the seam for the two GitHub skill names of the `skills` family:
//
//	skills:inspect-github   args [sourceUrl]  -> {success, inspection?}
//	skills:install-github   args [sourceUrl]  -> {success, skill?}
//
// Its verdict is a refusal, and the refusal is the EGRESS POLICY, not the
// package validation. The publication half of the port is already built and
// already used by the sibling local lane; the fetch half has no client this
// lane is allowed to dial with.
//
// # The wire shape, from the reference
//
//	src/shared/ipc-channels.ts:717-724
//	  'skills:inspect-github': { args: [sourceUrl: string]
//	    return: { success: boolean; inspection?: RemoteWritingSkillInspection; error?: string } }
//	  'skills:install-github': { args: [sourceUrl: string]
//	    return: { success: boolean; skill?: InstalledWritingSkill; error?: string } }
//
// Both take ONE string and nothing else: not a digest, not a candidate id, not a
// host path. The install half is therefore only safe if the main process keeps
// the verdict of the preceding inspection keyed by that same string
// (electron/controllers/app-data-controller.ts:446-449), refuses the install
// when no inspection is stored (:462-468), spends the receipt before refetching
// (:469-471), and re-fetches, comparing the digest and the resolved URL
// (:472-480). That is exactly what internal/promptskills already implements:
// the receipt key is the caller plus the source URL (remote.go:16), the
// missing-receipt sentence is byte-identical (:19, app-data-controller.ts:466),
// and the changed-after-inspection sentence is byte-identical (:48,
// app-data-controller.ts:478).
//
// # What refuses them now
//
// The registry already inventories both names (web/operations.json:317-318,
// scope "global") and no binder installs a handler, so each one answers 501
// GO_OPERATION_NOT_MIGRATED from internal/transport/server.go:116. The skills
// binder beside them serves only the other five names
// (internal/transport/promptskills.go:19), which is why the family looks
// complete and is not.
//
// # The two ports that exist
//
//	promptskills.InspectGitHub  internal/promptskills/remote.go:40
//	promptskills.InstallGitHub  internal/promptskills/remote.go:47
//
// Both are faithful ports of the reference, and both are unreachable here,
// before any validation runs, for three reasons that are all about the client.
//
//  1. promptskills.GitHubDispatcher (internal/promptskills/remote.go:26) has no
//     implementation. The package says why in its own words: "There is
//     deliberately no default http.Client or process-wide default transport"
//     (:22-25), and the interface "MUST NOT follow redirects or broaden these
//     two origins" (:25). This lane's only admitted outbound client is
//     (*egress.Client).Do (internal/egress/client.go:120), whose origin set is
//     the committed model policy: production_model_egress_linux.go:58 builds it
//     from config.json's modelOrigins, else from the baseUrl of every committed
//     model profile (:62, :125-145). The only way a GitHub origin gets into that
//     set is an operator writing one into a MODEL allowlist, which no shipped
//     config does (the committed fixture names its model endpoint only), so a
//     dispatch to https://api.github.com/... or
//     https://raw.githubusercontent.com/... is refused by the policy itself as
//     egress.ErrNotAllowed (internal/egress/client.go:27, returned at :196).
//     A dispatcher built here would be a handler that answers nothing but that
//     refusal - which is the one thing this lane has decided not to install
//     (production_open_linux.go:559-561).
//  2. promptskills.RemoteAuthority (internal/promptskills/remote.go:30) has no
//     implementation either, and SetRemoteAuthority (:31) is mandatory: while it
//     is unset InspectGitHub refuses at :41 and InstallGitHub at :48 with
//     ErrRemoteUnavailable (:20), whose value is literally
//     APPROVED_GITHUB_DISPATCH_UNAVAILABLE. Its contract is a hold on
//     "the application's current-generation admission read lock" across the
//     publication, so that a checked generation cannot go stale between the
//     digest comparison and the write; this lane pins one committed generation
//     for the process lifetime and exposes no read lease to lend
//     (globaladmission.PinnedReady takes an exclusive mutex,
//     internal/globaladmission/pinned_linux.go:257-261), so a lease minted here
//     would be a lock no lifecycle would honour.
//  3. The reference does the fetch in the desktop main process with the
//     runtime's own client, and sets redirect: 'error' by hand
//     (electron/controllers/app-data-controller.ts:264-285, 10 s budget,
//     content-length plus byte bound, then inspectWritingSkillMarkdown). It does
//     NOT fetch through the renderer, so there is nothing to copy from there
//     either: reproducing it means adding a second outbound path beside the
//     committed one, which is the thing the lane's egress plan exists to stop.
//
// # What this file therefore does
//
// It computes the refusal instead of asserting it. admission() asks the
// committed policy the same question a dispatcher would have to ask -
// (*egress.Client).CheckBaseURL (internal/egress/client.go:36) over the exact
// two origins (internal/promptskills/remote.go:35-36) - and reports which wall
// was hit: no policy at all, a policy that does not admit github, or a policy
// that would admit it but still has no dispatcher or authority. Nothing is
// bound in any of the three cases, because a name that cannot answer must stay
// at the registry's own 501 rather than move a count it cannot justify.
//
// The evidence is a constant, not a comment: productionSkillGitHubRefusalEvidence
// is a list of "path:line symbol" citations, and the test beside this file
// re-resolves every one of them, so the day a dispatcher or an authority lands,
// the refusal points at the next missing thing instead of at a stale claim.
//
// # What would unblock it
//
// Three things, in this order, and none of them is validation:
//
//	1. an origin the policy admits   either the operator commits both GitHub
//	                                 origins, or the lane gains a purpose-built
//	                                 policy for them - not a second outbound
//	                                 path invented in a handler.
//	2. a GitHubDispatcher            FetchGitHub over that admitted client only,
//	                                 reconstructing the two endpoints from
//	                                 ParseGitHubURL's safe parts (remote.go:34-36
//	                                 already does), refusing any 3xx itself,
//	                                 reading at most MaxSkillBytes+1 and
//	                                 honouring ctx.
//	3. a RemoteAuthority             a lease on the committed generation that a
//	                                 lifecycle rollover really waits for.
//
// Until then the honest answer is the registry's 501, which is what both names
// give today.
const (
	// productionSkillGitHubInspectName and productionSkillGitHubInstallName are
	// the two names, spelled as the renderer sends them.
	productionSkillGitHubInspectName = "skills:inspect-github"
	productionSkillGitHubInstallName = "skills:install-github"
)

// The refusal vocabulary. Each code is symbolic and fixed: no host, no path, no
// committed value and no os sentence is ever formatted into one. They are
// logged, never returned on the wire, because neither name is bound.
const (
	// productionSkillGitHubLaneCode: there is no lane to fetch for (a nil lane,
	// or a nil server the binder could not install into).
	productionSkillGitHubLaneCode = "SKILLS_GITHUB_LANE_UNAVAILABLE"
	// productionSkillGitHubEgressCode: the committed generation names no usable
	// egress origin, so there is not even a policy to ask.
	productionSkillGitHubEgressCode = "SKILLS_GITHUB_EGRESS_UNAVAILABLE"
	// productionSkillGitHubOriginCode: the committed policy exists and does not
	// admit the GitHub origins the port would dispatch to.
	productionSkillGitHubOriginCode = "SKILLS_GITHUB_ORIGIN_NOT_ADMITTED"
	// productionSkillGitHubPortCode: the committed policy would admit both
	// origins, and the two ports are still missing.
	productionSkillGitHubPortCode = "SKILLS_GITHUB_PORT_UNAVAILABLE"
)

// productionSkillGitHubRefusalEvidence is the refusal's proof, as citations a
// test can check: "path:line symbol", semicolon-separated, resolved from the
// backend-go module root. The formatter never reorders or renames any of it, so
// a failure to resolve means the declaration really moved, changed or was
// implemented - and the seam, not the test, is what has to be rewritten then.
const productionSkillGitHubRefusalEvidence = "" +
	"internal/promptskills/remote.go:26 GitHubDispatcher; " +
	"internal/promptskills/remote.go:30 RemoteAuthority; " +
	"internal/egress/client.go:27 ErrNotAllowed; " +
	"cmd/novel-server/production_model_egress_linux.go:62 ModelEgressOrigins"

// productionSkillGitHubOrigins is the exact pair the port dispatches to: the
// repository lookup and the raw file download, both hardcoded https in
// internal/promptskills/remote.go:35-36, never a host parsed out of the request.
// It is not a policy and is never handed to egress.New - it is only the question
// this seam asks the committed policy.
var productionSkillGitHubOrigins = []string{
	"https://api.github.com",
	"https://raw.githubusercontent.com",
}

// productionSkillGitHub is the admission half of the two names, and nothing
// else: it owns no root, keeps no receipt, holds no connection and starts no
// request. Its single field is the lane's own client accessor
// (production_open_linux.go:379), so a test can hand it a policy built by hand
// and see which of the three refusals comes back.
type productionSkillGitHub struct {
	policy func() (*egress.Client, error)
}

// newProductionSkillGitHub returns the seam for the lane built by
// productionHandlerWithContract. A nil lane returns nil, which the binder
// refuses by name instead of dereferencing.
func newProductionSkillGitHub(open *productionOpen) *productionSkillGitHub {
	if open == nil {
		return nil
	}
	return &productionSkillGitHub{policy: open.ModelEgress}
}

// admission asks the committed policy the question a dispatcher would have to
// answer, and reports the first wall that refuses. It never widens the policy:
// the two origins are only ever passed to CheckBaseURL, which parses and
// compares, performs no DNS lookup and starts no request
// (internal/egress/client.go:33-55).
func (g *productionSkillGitHub) admission() error {
	if g == nil || g.policy == nil {
		return errors.New(productionSkillGitHubLaneCode)
	}
	policy, err := g.policy()
	if err != nil || policy == nil {
		// A committed generation with no model origin has no policy at all; the
		// refusal never formats the policy's own error, which may carry a
		// committed endpoint.
		return errors.New(productionSkillGitHubEgressCode)
	}
	for _, origin := range productionSkillGitHubOrigins {
		if checkErr := policy.CheckBaseURL(origin); checkErr != nil {
			return errors.New(productionSkillGitHubOriginCode)
		}
	}
	// Both origins are admitted, and the dispatch still cannot happen: the
	// dispatcher and the authority are ports, not policy.
	return errors.New(productionSkillGitHubPortCode)
}

// Bind is where the two handlers WOULD be installed, and it installs neither.
// It exists so a wiring attempt is loud at startup and leaves the two names at
// 501 GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116) instead of
// counting them as served. The refusal is logged with its evidence and returned;
// no operation name, no argument and no host path is ever part of the message.
func (g *productionSkillGitHub) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New(productionSkillGitHubLaneCode)
	}
	refusal := g.admission()
	log.Printf("novel-server: %s and %s stay unbound: %v (%v)", productionSkillGitHubInspectName,
		productionSkillGitHubInstallName, refusal, productionSkillGitHubRefusalEvidence)
	return refusal
}

// bindProductionSkillGitHub is the one call a parent would add beside the
// sibling binder (production_open_linux.go:512, bindProductionSkillInstall). It
// binds nothing today, so the honest wiring is NOT to call it: adding it can
// only log the refusal, and adding it while ignoring the error would suggest a
// port exists. It is here so the decision has one address in the wiring, and so
// the test can prove the refusal is the seam's answer and not an omission.
func bindProductionSkillGitHub(server *transport.Server, lane *productionSkillGitHub) error {
	if lane == nil {
		return errors.New(productionSkillGitHubLaneCode)
	}
	return lane.Bind(server)
}
