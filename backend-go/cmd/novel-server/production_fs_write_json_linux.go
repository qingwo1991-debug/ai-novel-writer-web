//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's fs:write-json. It publishes one name and
// nothing else, and it is the confined write counterpart of the file surface
// this lane already serves (production_fs_reads_linux.go).
//
// # The reference it ports
//
// The desktop main process registered the name as
//
//	ipcMain.handle('fs:write-json', async (
//	  _event, context: ProjectSessionContext, filePath: string,
//	  data: unknown, expectedProjectPath: string,
//	)
//
// (electron/controllers/fs-controller.ts:338-344), then answered it after
// `JSON.stringify(data, null, 2)` -> `writeTextAtomically` (:351) with
//
//	{ success: true }                          // :355
//	{ success: false, error: ... }             // :117-125, :357-359
//
// The wire shape is the type the browser declares for the name -
// [filePath, data, expectedProjectPath] (src/shared/ipc-channels.ts:484-487) -
// with the frozen three-key session context appended, which is exactly how its
// sibling fs:write-file is spelled here (internal/transport/project_files.go:44):
//
//	fs:write-json  arity 4  [relativePath, document, claimedRoot, context]
//
// The desktop's two real callers name an ABSOLUTE path inside the project
// (src/services/workflows/commands/architecture.command.ts:644-649,
// src/services/workflows/chapter-write-launch.ts:130-134). This lane refuses
// that spelling by name and serves the RELATIVE path the rest of the browser
// file contract already requires, for the reason
// internal/projectfs/browser_files.go:38-52 gives: an absolute-path route makes
// the confinement depend on one comparison being right, and it lets a page
// address anything the server user can reach. A renderer that wants this name
// must send the project-relative spelling, exactly as it must for fs:write-file.
//
// # Why the family binder is not used
//
// transport.BindProjectFiles publishes fs:read-file, fs:mkdir, fs:check-exists
// and fs:write-file atomically (internal/transport/project_files.go:86-94).
// This lane already serves the first three (production_fs_reads_linux.go:220-237)
// and fs:write-file through the write batch
// (production_config_write_linux.go:543-559), so taking that binder would fail
// startup with OPERATION_ALREADY_BOUND instead of publishing this name. The name
// is bound once with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-77), which itself refuses a name that is not
// inventoried (OPERATION_NOT_EXPOSED) or already bound (OPERATION_ALREADY_BOUND).
//
// # The four properties this name keeps, in order
//
//  1. JSON before bytes. productionJSONDocument encodes the wire value the way
//     the desktop did - two-space indent, no trailing newline, no HTML escaping -
//     and then proves the exact bytes are one JSON document with json.Valid. Both
//     steps run before any project is borrowed, so bytes that are not a JSON
//     document can never reach the tree.
//  2. Atomic publication. The bytes are committed by
//     projectfs.Project.WriteBrowserFile (internal/projectfs/browser_files.go:
//     177-210): one temporary file in the already pinned parent directory, one
//     Sync, a type/mode re-check, one rename, and a readback compared byte for
//     byte (browser_files.go:215-254). A concurrent reader sees the old document
//     or the new one, never a truncated one.
//  3. A bounded document. productionJSONMaxBytes is checked on the ENCODED bytes
//     before the project is borrowed, and projectfs re-checks its own 64 MiB
//     ceiling on the bytes it commits (browser_files.go:181-183).
//  4. A refusal writes nothing. The arity, the frozen context, the claimed root
//     and the path are validated by the read side's own parser
//     (production_fs_reads_linux.go:495-522) before the document is encoded, and
//     productionFSParseFileRequest confines the path through
//     projectfs.BrowserRelativePath (browser_files.go:73-84), which refuses an
//     absolute path, a traversal, a hidden component and the storage-reserved
//     names BEFORE anything is opened. A symlink anywhere in the path is refused
//     by the pinned walk (internal/projectfs/project.go:62-82). Nothing that is
//     refused here can create, replace or remove a byte.
//
// One asymmetry is deliberate and inherited: the storage directory is dot
// prefixed, so .ai-novel/partial_arch.json - the one document the desktop's
// callers wrote - is refused by name. fs:read-json serves that marker read-only
// under its own fixed path (production_fs_reads_linux.go:79-98); a browser still
// cannot write it, and that is the property this lane already chose.
//
// The confinement itself is not reimplemented here. The lease fence
// (productionFSFence), the pinned tree walk and the atomic replacement are the
// ones fs:read-file and fs:write-file already use, through the same
// productionFSReads adapter.

const (
	// productionFSWriteJSONName is the single mutating fs:* name this file serves.
	productionFSWriteJSONName = "fs:write-json"

	// The wire shape: the document sits between the path and the claimed root,
	// exactly where fs:write-file keeps its body
	// (internal/transport/project_files.go:44), and the frozen three-key session
	// context is last (internal/transport/project_files.go:46-51).
	productionFSWriteJSONArity   = 4
	productionFSWriteJSONPathPos = 0
	productionFSWriteJSONDataPos = 1
	productionFSWriteJSONRootPos = 2
	productionFSWriteJSONCtxPos  = 3

	// productionJSONMaxBytes bounds the encoded document. It is deliberately
	// below the transport's own request envelope (internal/security/body.go:15):
	// a document that filled the whole body would be refused by the transport as
	// BODY_TOO_LARGE, which is not this name's refusal, and the rest of the frame
	// - the path, the claimed root and the frozen context - still has to fit.
	// A document over this ceiling is refused here, as PROJECT_FILE_TOO_LARGE,
	// before the project is borrowed.
	productionJSONMaxBytes = security.DefaultBodyLimit / 2
)

// productionFSWriteJSON is the document-write adapter. It owns no path, no root
// and no handle: it lends the read side's confined adapter, which re-observes the
// caller's live lease and pins the project for its own request.
type productionFSWriteJSON struct {
	reads *productionFSReads
}

// newProductionFSWriteJSON returns the adapter for the lane built by
// productionHandlerWithContract. Nothing is resolved, opened or bound at
// construction time.
func newProductionFSWriteJSON(open *productionOpen) *productionFSWriteJSON {
	return &productionFSWriteJSON{reads: newProductionFSReads(open)}
}

// Bind installs fs:write-json. A nil adapter or a nil server is refused instead
// of being published as a handler that can only fail, and server.Bind refuses an
// uninventoried or already bound name, so a name this file should not publish is
// a startup error rather than silent dead code.
func (w *productionFSWriteJSON) Bind(server *transport.Server) error {
	if w == nil || w.reads == nil || server == nil {
		return errors.New("PRODUCTION_FS_WRITE_JSON_BINDER_INVALID")
	}
	return server.Bind(productionFSWriteJSONName, w.writeJSON)
}

// writeJSON serves fs:write-json: one JSON document, one relative path inside the
// caller's own project, under the caller's live lease.
//
// The order is the contract. The wire shape is checked first, then the path and
// the frozen context through the read side's validator (which confines the path
// before anything is opened), then the document is encoded and proved to be JSON,
// then it is bounded, and only then is the project borrowed and the bytes
// committed atomically. Every refusal keeps the family's business envelope at
// HTTP 200 with one symbolic code, because the desktop answered this name that
// way and the renderer branches on `success`
// (electron/controllers/fs-controller.ts:117-125,
// internal/transport/project_files.go:201-209).
func (w *productionFSWriteJSON) writeJSON(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if w == nil || w.reads == nil {
		return productionJSONWriteFailure(productionFSClosedCode), nil
	}
	if len(args) != productionFSWriteJSONArity {
		return productionJSONWriteFailure("INVALID_ARGUMENTS"), nil
	}
	request, code := productionFSParseFileRequest([]any{
		args[productionFSWriteJSONPathPos],
		args[productionFSWriteJSONRootPos],
		args[productionFSWriteJSONCtxPos],
	})
	if code != "" {
		return productionJSONWriteFailure(code), nil
	}
	document, err := productionJSONDocument(args[productionFSWriteJSONDataPos])
	if err != nil {
		return productionJSONWriteFailure(productionFSCode(err)), nil
	}
	if int64(len(document)) > productionJSONMaxBytes {
		return productionJSONWriteFailure("PROJECT_FILE_TOO_LARGE"), nil
	}
	if err := w.reads.withProjectFile(ctx, caller, request.expected, func(project *projectfs.Project) error {
		return project.WriteBrowserFile(ctx, request.relative, document)
	}); err != nil {
		return productionJSONWriteFailure(productionJSONWriteCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// productionJSONDocument encodes one wire value exactly the way the desktop wrote
// it - JSON.stringify(data, null, 2) (electron/controllers/fs-controller.ts:351)
// - and then proves the bytes really are one JSON document.
//
// The encode turns an arbitrary argument value into bytes; json.Valid is the
// second, independent check on those exact bytes, so nothing unparsable can reach
// a project tree even if the encoder is ever changed. Two details match the
// reference: HTML characters are not escaped (JSON.stringify leaves <, > and &
// alone) and there is no trailing newline (Encoder.Encode appends one).
//
// An unencodable value - the wire layer rejects NaN and Infinity before this
// point (internal/security/body.go:103-143), but a future caller may hand one in
// directly - is refused as INVALID_ARGUMENTS rather than written as anything.
func productionJSONDocument(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	document := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if !json.Valid(document) {
		return nil, security.NewHTTPError(http.StatusInternalServerError, "PROJECT_JSON_INVALID")
	}
	// The buffer is copied so the returned document never aliases it.
	return append([]byte(nil), document...), nil
}

// productionJSONWriteCode maps a refused commit to one symbolic code. A
// replacement that may already be the project's bytes is named rather than
// reported as a plain failure, exactly like the sibling fs:write-file
// (production_config_write_linux.go:631-640): ErrBrowserCommitUnknown is a
// joined value (internal/projectfs/browser_files.go:33-35), so it is checked
// first.
func productionJSONWriteCode(err error) string {
	if errors.Is(err, projectfs.ErrBrowserCommitUnknown) {
		return "PROJECT_FILE_COMMIT_UNKNOWN"
	}
	return productionFSCode(err)
}

// productionJSONWriteFailure is the family's business refusal envelope for this
// name: `{success: false, error: CODE}`, which is how the desktop answered it
// (electron/controllers/fs-controller.ts:117-125, read at :357-359). It carries
// no commitState because only fs:write-file has one (:121-123). The code is a
// symbolic constant or an HTTPError code and never a host path, so no wrapped
// path, os error sentence or SQLite message can reach the browser.
func productionJSONWriteFailure(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}
