//go:build linux

package main

import (
	"errors"
	"io/fs"
	"log"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/promptskills"
)

// This file serves `skills:install-local` - the last refusing name of the two
// local skill names the renderer still sends in web mode
// (src/services/ipc-client.ts:38-45 posts them like any operation).
//
// # What refused, and why it now does not
//
// `skills:pick-local` already worked: it reads the staged upload through the
// picker's upload seam, judges it with promptskills' own local validator and
// mints a single-use candidate bound to the caller
// (production_browser_pickers_linux.go:1192-1352). The confirm step then reached
// `productionPickerSkillPublish` (`p.publish`, :1324-1330), which was nil because
// internal/promptskills exported no local publication port, and answered
// SKILLS_LOCAL_INSTALL_UNAVAILABLE (production_upload_route_linux.go:80-87).
//
// Two things close it, and neither is a second validation path:
//
//	promptskills.InstallLocal   publishes bytes under the same gate the GitHub
//	                            install passes (internal/promptskills/local.go)
//	productionSkillInstalls     lends the picker that port over the SAME prompt
//	                            and skill root the prompt reads and writes use
//
// # What the install reuses from the GitHub path
//
// Everything that decides whether a package may be published:
//
//	the byte gate        bounded by MaxSkillBytes, valid UTF-8, no NUL byte
//	the verdict          `inspect` (inspection.go:64-77) - the one function the
//	                     GitHub verdict is made of; a relative reference, a
//	                     script, a hook, a subagent or a tool dependency still
//	                     makes a package incompatible
//	the name gate        promptskills.ValidName, and the declared frontmatter name
//	                     must be the name presented
//	the publish shape    SKILL.md alone, no bundle manifest - the shape
//	                     InstallGitHub publishes (remote.go:48)
//	the atomic write     install_linux.go:14-16: a private staging directory, one
//	                     rename, an existing name refuses, a failure removes the
//	                     staging directory
//
// # What is deliberately NOT reused
//
// GitHub's inspection receipt and its RemoteAuthority lease. Those exist to make
// a NETWORK verdict trustworthy between two requests; here the candidate already
// carries the digest of the bytes the picker read, and the picker re-reads and
// re-digests the staged upload before it calls Publish
// (production_browser_pickers_linux.go:1300-1352), so there is nothing to
// re-fetch and no remote generation to pin.
//
// # The one line the parent adds
//
// production_open_linux.go:507 currently reads
//
//	if bindErr := newProductionBrowserPickersFrom(open, uploads).Bind(server); bindErr != nil {
//
// and becomes
//
//	pickers := newProductionBrowserPickersFrom(open, uploads)
//	if bindErr := bindProductionSkillInstall(pickers, newProductionSkillInstalls(open)); bindErr != nil {
//		_ = open.Close()
//		return nil, nil, bindErr
//	}
//	if bindErr := pickers.Bind(server); bindErr != nil {
//
// No operation name is bound by this file: `skills:install-local` is already
// bound by the picker family's own Bind
// (production_browser_pickers_linux.go:483-508), so the served-operation count
// does not move for this name.

// The refusal vocabulary of the publisher. Every code is symbolic and every one
// of them is only ever logged: the picker maps any publisher error to its own
// SKILLS_INSTALL_FAILED verdict, which is the only thing the page reads
// (production_browser_pickers_linux.go:1324-1330). No host path, no os error
// sentence and no candidate id reaches either the log or the wire.
const (
	// productionSkillInstallLaneCode: this lane cannot publish at all (no lane
	// value, e.g. a publisher built for a closed open).
	productionSkillInstallLaneCode = "SKILLS_LOCAL_INSTALL_LANE_UNAVAILABLE"
	// productionSkillInstallRootCode: the committed generation root could not be
	// re-proved for this publication.
	productionSkillInstallRootCode = "SKILLS_LOCAL_INSTALL_ROOT_UNAVAILABLE"
	// productionSkillInstallExistsCode: the name is already installed, so the
	// publication is refused rather than overwritten.
	productionSkillInstallExistsCode = "SKILLS_LOCAL_INSTALL_EXISTS"
	// productionSkillInstallPackageCode: promptskills refused the package, or
	// refused to write it, for a reason whose own code is named in the log.
	productionSkillInstallPackageCode = "SKILLS_LOCAL_INSTALL_REFUSED"
)

// productionSkillInstalls is the local skill publisher: the `publish` seam the
// picker family calls after it has re-read and re-verified the staged bytes
// (production_browser_pickers_linux.go:1290-1352).
//
// It owns no root of its own. It borrows productionPromptWrites' resolution
// (production_config_write_linux.go:497-528: the committed generation, re-proved
// with a timeout, cached only after it resolved once), because the installed
// skills and the prompt templates are the two trees of the SAME generation and a
// second resolution would be a second answer to "where does a skill live".
type productionSkillInstalls struct {
	open    *productionOpen
	prompts *productionPromptWrites
}

// newProductionSkillInstalls returns the publisher for the lane built by
// productionHandlerWithContract. A nil lane returns nil, which the binder
// refuses, so a wiring mistake is loud instead of a nil dereference later.
func newProductionSkillInstalls(open *productionOpen) *productionSkillInstalls {
	if open == nil {
		return nil
	}
	return &productionSkillInstalls{open: open, prompts: &productionPromptWrites{open: open}}
}

// bindProductionSkillInstall installs the publisher on the picker family. It is
// the whole wiring: the picker's candidate table, its upload seam and its
// refusals are untouched, and only the one field that was deliberately nil is
// given a value.
func bindProductionSkillInstall(pickers *productionBrowserPickers,
	installs *productionSkillInstalls) error {
	if pickers == nil || installs == nil || installs.prompts == nil {
		return errors.New("PRODUCTION_SKILL_INSTALL_BINDER_INVALID")
	}
	pickers.publish = installs.Publish
	return nil
}

// Publish is the picker's publish seam: install one re-verified SKILL.md under
// the name the picker showed the author. It opens a fresh service per publish -
// promptskills.New re-opens every ancestor without following a symlink and
// re-checks the owner and the mode - and releases it before it returns
// (production_singleton_reads_linux.go:312-320 does the same for a read).
func (s *productionSkillInstalls) Publish(name, raw string) error {
	if s == nil || s.open == nil || s.prompts == nil {
		log.Printf("novel-server: %s refused: %s", productionPickerSkillInstall,
			productionSkillInstallLaneCode)
		return errors.New(productionSkillInstallLaneCode)
	}
	root, err := s.prompts.pinnedRoot()
	if err != nil {
		log.Printf("novel-server: %s refused: %s", productionPickerSkillInstall,
			productionSkillInstallRootCode)
		return errors.New(productionSkillInstallRootCode)
	}
	service, err := promptskills.New(root)
	if err != nil {
		log.Printf("novel-server: %s refused: %s", productionPickerSkillInstall,
			productionSkillInstallRootCode)
		return errors.New(productionSkillInstallRootCode)
	}
	defer func() { _ = service.Close() }()
	installed, err := service.InstallLocal(name, raw)
	if err != nil {
		// The package's own codes are symbolic (PROMPT_SKILL_*, SOURCE_INVALID,
		// ...), but an os error underneath them is not, so only a known code is
		// forwarded and everything else collapses to one refusal.
		log.Printf("novel-server: %s refused: %s", productionPickerSkillInstall,
			productionSkillInstallReason(err))
		return errors.New(productionSkillInstallPackageCode)
	}
	log.Printf("novel-server: %s published %s (source=%s, %d bytes)", productionPickerSkillInstall,
		installed.Name, installed.Source, installed.UTF8Bytes)
	return nil
}

// productionSkillInstallReason is the log-only mapping of a publication refusal:
// one symbolic code, chosen from the closed set this port can produce. It exists
// so an os error, whose text may carry a path, can never be printed.
func productionSkillInstallReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrExist):
		return productionSkillInstallExistsCode
	case errors.Is(err, promptskills.ErrClosed):
		return promptskills.ErrClosed.Error()
	case errors.Is(err, promptskills.ErrUnsafe):
		return promptskills.ErrUnsafe.Error()
	case errors.Is(err, promptskills.ErrChanged):
		return promptskills.ErrChanged.Error()
	case errors.Is(err, promptskills.ErrUnsupported):
		return promptskills.ErrUnsupported.Error()
	case errors.Is(err, promptskills.ErrLocalIncompatible):
		return promptskills.ErrLocalIncompatible.Error()
	case errors.Is(err, promptskills.ErrLocalNameChanged):
		return promptskills.ErrLocalNameChanged.Error()
	case errors.Is(err, promptskills.ErrPackageInvalid):
		return promptskills.ErrPackageInvalid.Error()
	case errors.Is(err, promptskills.ErrPackageLarge):
		return promptskills.ErrPackageLarge.Error()
	case errors.Is(err, promptskills.ErrPackageMissing):
		return promptskills.ErrPackageMissing.Error()
	case errors.Is(err, promptskills.ErrPackageSource):
		return promptskills.ErrPackageSource.Error()
	case errors.Is(err, promptskills.ErrPackageSymlink):
		return promptskills.ErrPackageSymlink.Error()
	}
	return productionSkillInstallPackageCode
}
