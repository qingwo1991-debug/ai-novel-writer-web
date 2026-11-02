//go:build !linux

package projectfs

import "io/fs"

func knowledgePrivate(info fs.FileInfo) bool     { return false }
func knowledgePrivateFile(info fs.FileInfo) bool { return false }
