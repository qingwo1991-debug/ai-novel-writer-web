//go:build linux

package promptskills

import (
	"errors"
	"strings"
)

// This file is the LOCAL publish port. Until now the package could only publish
// a skill it had itself downloaded: InstallGitHub (remote.go:47-49) was the one
// exported installer, and install (install_linux.go:14-16) is unexported and
// refuses its `local` flag on purpose, because a package root is a staging
// directory and never browser authority.
//
// A browser upload route now stages the author's own SKILL.md (or folder) and
// hands the bytes to the picker, which inspects them with this package's own
// local validator and mints a single-use candidate bound to the caller
// (cmd/novel-server/production_browser_pickers_linux.go:1192-1352). The only
// thing missing was the publication half: a caller holding already verified
// BYTES had no way to publish them, so the confirm step answered
// SKILLS_LOCAL_INSTALL_UNAVAILABLE (production_upload_route_linux.go:80-87).
//
// InstallLocal is that port. It carries bytes, never a path: no argument is a
// filesystem location, so there is no prefix to confine and no grant to honour.
// Everything it publishes is re-validated here, on the same `inspect` core the
// GitHub verdict is made of (inspection.go:64-77), so a portable package is
// either acceptable to both lanes or to neither.

// ErrLocalIncompatible is the refusal of a local SKILL.md that is not a
// self-contained prompt skill: the same verdict the GitHub lane reaches before
// it publishes (InstallGitHub's `inspection.Compatible` check, remote.go:48).
// It is a code because the caller maps it, and it is deliberately distinct from
// ErrInspectFirst: nothing is "inspected first" here, the bytes themselves were
// judged.
var ErrLocalIncompatible = errors.New("PROMPT_SKILL_INCOMPATIBLE")

// ErrLocalNameChanged is the refusal of a publication whose frontmatter names a
// different skill than the name the caller presented. The name is the install
// target directory, so a package must not choose its own address after the
// picker showed the author one.
var ErrLocalNameChanged = errors.New("PROMPT_SKILL_NAME_CHANGED")

// InstallLocal publishes one already-staged SKILL.md under name and returns the
// installed skill. The caller is the package owner: it staged the bytes, it
// inspected them, and it holds a single-use candidate for exactly this name and
// exactly these bytes (the picker re-reads and re-digests before it calls here,
// production_browser_pickers_linux.go:1300-1352). Nothing in this function
// trusts that: the bytes are bounded and re-inspected, and the published shape
// is the shape the GitHub path publishes - SKILL.md alone, no bundle manifest
// (install_linux.go:11-14), which is also why the picker refuses a package with
// bundled files before it mints a candidate (:1254-1261).
//
// A service that is closed refuses without touching the root, and an existing
// name refuses with fs.ErrExist from the same install gate the remote path uses
// (install_linux.go:15): a damaged or duplicate publication writes nothing, so
// the skill root is byte-identical after every refusal.
func (s *Service) InstallLocal(name string, raw string) (InstalledSkill, error) {
	if s == nil {
		return InstalledSkill{}, ErrClosed
	}
	// The name is the install directory. It is validated here even though the
	// caller validated it, because install() is also reachable from InstallGitHub
	// and this port must not be the weaker entrance.
	if !ValidName(name) {
		return InstalledSkill{}, ErrUnsafe
	}
	// The same byte gate the remote download path applies: bounded by
	// MaxSkillBytes, valid UTF-8 and free of NUL (remote.go:36, :48).
	if !validText([]byte(raw)) {
		return InstalledSkill{}, ErrPackageInvalid
	}
	// The verdict is the picker's own: InspectLocal with no bundle reader and no
	// fallback judges the frontmatter, the structure, the size and the references
	// that would leave the package, and cannot be argued with by the caller.
	inspection := InspectLocal(raw, nil, "")
	if !inspection.Compatible {
		return InstalledSkill{}, ErrLocalIncompatible
	}
	// A frontmatter name, when the package has one, is the name it declares; a
	// package named `helper` may not be installed as `daily-review`. A package
	// with no name field is named by the picker from its file or folder name,
	// which is the one case where the two legitimately differ.
	fields, _ := frontmatter(raw, true)
	if declared := strings.TrimSpace(fields["name"]); declared != "" && declared != name {
		return InstalledSkill{}, ErrLocalNameChanged
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return InstalledSkill{}, ErrClosed
	}
	if err := s.install(raw, name, nil, false); err != nil {
		return InstalledSkill{}, err
	}
	return InstalledSkill{
		Name:       name,
		Source:     "user",
		Version:    inspection.Metadata.Version,
		Language:   inspection.Metadata.Language,
		Compatible: true,
		UTF8Bytes:  inspection.UTF8Bytes,
	}, nil
}
