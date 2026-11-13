//go:build linux

package promptskills

import (
	"errors"
	"io/fs"
	"syscall"
)

// install publishes a complete staging directory. The normal remote install
// stores only SKILL.md and deliberately no bundle manifest, as TypeScript does.
// Local staged packages are not a public path-based API.
func (s *Service) install(raw, name string, bundle []BundledFile, local bool) error {
	if local || len(bundle) > 0 {
		return errors.New("TRUSTED_LOCAL_UPLOAD_GRANT_UNAVAILABLE")
	}
	dir, err := openChildDir(s.root, "skills", true, true)
	if err != nil {
		return err
	}
	defer dir.close()
	if _, err = statAt(dir, name); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	temp, err := randomName(".install-")
	if err != nil {
		return err
	}
	if err = syscall.Mkdirat(fd(dir), temp, 0700); err != nil {
		return err
	}
	defer removeTree(dir, temp)
	staging, err := openChildDir(dir, temp, false, true)
	if err != nil {
		return err
	}
	err = atomicWrite(staging, "SKILL.md", []byte(raw), true)
	staging.close()
	if err != nil {
		return err
	}
	if _, err = statAt(dir, name); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = syscall.Renameat(fd(dir), temp, fd(dir), name); err != nil {
		return err
	}
	return dir.file.Sync()
}
