//go:build !linux

package promptskills

import (
	"errors"
	"io/fs"
)

// Descriptor nofollow and link-identity semantics are currently Linux-only.
var ErrUnsupported = errors.New("PROMPT_SKILLS_PLATFORM_UNSUPPORTED")
var ErrUnsafe = errors.New("PROMPT_SKILL_PATH_UNSAFE")
var ErrChanged = errors.New("PROMPT_SKILL_IDENTITY_CHANGED")

type ownedDir struct{}

func (*ownedDir) close() error                                      { return nil }
func openOwnedRoot(string) (*ownedDir, error)                       { return nil, ErrUnsupported }
func openChildDir(*ownedDir, string, bool, bool) (*ownedDir, error) { return nil, ErrUnsupported }
func statAt(*ownedDir, string) (fs.FileInfo, error)                 { return nil, ErrUnsupported }
func safeFile(fs.FileInfo) bool                                     { return false }
func readAt(*ownedDir, string, int) ([]byte, error)                 { return nil, ErrUnsupported }
func readRelative(*ownedDir, string, int) ([]byte, error)           { return nil, ErrUnsupported }
func atomicWrite(*ownedDir, string, []byte, bool) error             { return ErrUnsupported }
func removeFile(*ownedDir, string) error                            { return ErrUnsupported }
func entries(*ownedDir) ([]fs.DirEntry, error)                      { return nil, ErrUnsupported }
func removeTree(*ownedDir, string) error                            { return ErrUnsupported }
func randomName(string) (string, error)                             { return "", ErrUnsupported }
func (*Service) install(string, string, []BundledFile, bool) error  { return ErrUnsupported }
