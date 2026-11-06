//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's CHARACTER AVATAR slice. It binds exactly the
// four inventoried names (web/operations.json:51-54, all scope `project`) and
// nothing else:
//
//	character-avatar:choose      arity 1 or 2  [characterId] or [characterId, uploadId]
//	character-avatar:commit      arity 2       [characterId, base64]
//	character-avatar:read-batch  arity 1       [characterIds]
//	character-avatar:remove      arity 1       [characterId]
//
// # The reference
//
//	electron/controllers/character-avatar-controller.ts:81-142   the four handlers
//	electron/controllers/character-avatar-controller.ts:28-41    the failure envelope
//	src/shared/character-avatar.ts:1-61                           the wire shapes
//	electron/services/character-asset-service.ts:110-190          commit and readMany
//	electron/services/character-asset-service.ts:192-222          remove and reclaim
//	electron/repositories/character-asset-repository.ts:53-104    the row rules
//	electron/services/avatar-image.ts:6-14                        detectAvatarExtension
//	internal/admission/baseline_v7_ddl.go:100-108                 character_avatar_assets
//	src/components/editor/use-character-avatars.ts:41-63          how the renderer shows one
//	src/components/editor/use-character-avatar.ts:10-18            base64 -> object URL
//
// # An avatar is author identity data, so the answer is the row plus the bytes
//
// The desktop stored an asset as a file below the project's storage root plus one
// `character_avatar_assets` row (character_id PRIMARY KEY, asset_revision, the
// unique relative_path, content_hash, mime from a three-value CHECK, byte_size).
// That row is the roster's current avatar, so this lane writes both halves and
// reads both halves back: read-batch answers the roster's current avatars, never a
// copy of a request.
//
// # Where the renderer fetches the bytes: the operation answer, not a route
//
// character-avatar:read-batch is defined to answer `{characterId, assetRevision,
// mime, base64}` (src/shared/character-avatar.ts:28-33) and the renderer turns
// that base64 into a blob URL itself (use-character-avatar.ts:10-18). So no
// file-serving route is needed and none is invented here: the production mux
// mounts only /api/library and the operation server
// (production_open_linux.go:806-811), `/api/downloads/*` is refused as
// GO_ROUTE_NOT_MIGRATED (internal/transport/server.go:123), and there is no
// served author-data file route to borrow. The file below .ai-novel is the
// durable half of the asset; the wire's base64 is how the browser reads it.
//
// # choose: a browser has no picker, so the bytes must be uploaded first
//
// In the desktop the handler opened a native dialog and answered the chosen
// bytes' preview (character-avatar-controller.ts:81-96). A browser has no such
// dialog, so choose takes the identity of an already staged upload - the same
// upload path this repo already uses for every browser file: POST /api/uploads
// (internal/transferhttp/uploads.go:132-215), read back through the
// productionPickerUploads seam keyed by the caller's cookie session and tab
// (production_browser_pickers_linux.go:285-300). The id is an opaque token that
// only its own session+tab can redeem; no host path is ever accepted, and the
// staged file's *name* is used only as a display label, never as a path.
//
// The production lane currently installs no upload owner (`newProductionBrowserPickers`
// leaves uploads nil, production_browser_pickers_linux.go:183-190, and
// main.go:429-437 mounts the staging route in the synthetic lane only), so when
// no owner is supplied choose answers AVATAR_UPLOAD_UNAVAILABLE - a refusal that
// names the missing port - and never a fake success. Supplying the seam makes the
// whole flow real; the test does exactly that.
//
// A page that has the bytes already (its own <input type="file">) never needs
// choose: commit's wire is [characterId, base64] and it is the recording step.
//
// # A lying mime is caught by the magic bytes
//
// Both entry points that can carry bytes - the staged upload and the base64 body
// - are decided by productionAvatarImage, which looks at the first bytes only:
// PNG's eight-byte signature, JPEG's FF D8 FF, WebP's RIFF....WEBP. Nothing reads
// a declared content type, an extension, or a mime field from the request, and
// commit never trusts the preview choose produced: a base64 of text named
// avatar.png is IMAGE_FORMAT_INVALID, and so is the same text uploaded under that
// name. The size bound is the reference's own 4 MiB before decoding (so a huge
// body is refused without allocating it) and again after decoding.
//
// # The write order, and what a refusal costs
//
// File first, then the row, then the superseded file (character-asset-service.ts:
// 138-152): a row is only written after its bytes are durable, and a row whose
// file vanished is skipped by read-batch exactly as the desktop's readMany skips
// it. A failed row write removes the file it just installed, so a refused commit
// leaves the tree and the database as they were. Every path below the storage
// root is opened through os.Root with O_NOFOLLOW, and the storage directory's
// inode is re-verified after the write (productionKnowledgeStorage.verify).

const (
	productionAvatarChooseName = "character-avatar:choose"
	productionAvatarCommitName = "character-avatar:commit"
	productionAvatarReadName   = "character-avatar:read-batch"
	productionAvatarRemoveName = "character-avatar:remove"

	// productionAvatarDirectory is the desktop's root below .ai-novel
	// (character-asset-service.ts:141 builds `avatars/<hash>/<name>`).
	productionAvatarDirectory = "avatars"

	// productionAvatarMaxInputBytes is MAX_CHARACTER_AVATAR_INPUT_BYTES
	// (src/shared/character-avatar.ts:3).
	productionAvatarMaxInputBytes = 4 << 20

	// productionAvatarMaxBase64Bytes is the reference's own pre-decode bound,
	// Math.ceil(MAX * 4 / 3) + 4 (character-asset-service.ts:129).
	productionAvatarMaxBase64Bytes = (productionAvatarMaxInputBytes*4+2)/3 + 4

	// productionAvatarMaxBatch is readMany's cap (character-asset-service.ts:162).
	productionAvatarMaxBatch = 256

	// productionAvatarMaxNameBytes bounds the one filename component the writer
	// derives; it is a revision, a digest and an extension, so 96 is generous.
	productionAvatarMaxNameBytes = 96
)

// The wire codes. The first eight are the desktop's own list
// (src/shared/character-avatar.ts:5-13); the three upload codes are the browser's
// additions, because the desktop had no upload step to name. Every one is
// symbolic and is also the `message`, since the reference's localized sentences
// are the renderer's job and this lane never sends prose.
const (
	productionAvatarCodeInvalidCharacter  = "INVALID_CHARACTER_ID"
	productionAvatarCodeNotFound          = "CHARACTER_NOT_FOUND"
	productionAvatarCodeReadFailed        = "IMAGE_READ_FAILED"
	productionAvatarCodeFormatInvalid     = "IMAGE_FORMAT_INVALID"
	productionAvatarCodeTooLarge          = "IMAGE_TOO_LARGE"
	productionAvatarCodeSaveFailed        = "AVATAR_SAVE_FAILED"
	productionAvatarCodeUploadRequired    = "AVATAR_UPLOAD_REQUIRED"
	productionAvatarCodeUploadUnavailable = "AVATAR_UPLOAD_UNAVAILABLE"
	productionAvatarCodeUploadUnknown     = "AVATAR_UPLOAD_UNKNOWN"
)

// The MIME values the schema's CHECK constraint accepts
// (internal/admission/baseline_v7_ddl.go:106).
const (
	productionAvatarMimePNG  = "image/png"
	productionAvatarMimeJPEG = "image/jpeg"
	productionAvatarMimeWebP = "image/webp"
)

// The refusals. Each is a symbolic value; productionAvatarFailure is the only
// place that turns any of them into a wire answer.
var (
	errProductionAvatarInvalidCharacter  = errors.New(productionAvatarCodeInvalidCharacter)
	errProductionAvatarNotFound          = errors.New(productionAvatarCodeNotFound)
	errProductionAvatarReadFailed        = errors.New(productionAvatarCodeReadFailed)
	errProductionAvatarFormatInvalid     = errors.New(productionAvatarCodeFormatInvalid)
	errProductionAvatarTooLarge          = errors.New(productionAvatarCodeTooLarge)
	errProductionAvatarSaveFailed        = errors.New(productionAvatarCodeSaveFailed)
	errProductionAvatarUploadRequired    = errors.New(productionAvatarCodeUploadRequired)
	errProductionAvatarUploadUnavailable = errors.New(productionAvatarCodeUploadUnavailable)
	errProductionAvatarUploadUnknown     = errors.New(productionAvatarCodeUploadUnknown)
)

// productionAvatarCharacterPattern is the controller's `^[a-zA-Z0-9_-]{1,128}$`
// (character-avatar-controller.ts:54).
var productionAvatarCharacterPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// productionAvatarBase64Pattern is the reference's validBase64
// (character-asset-service.ts:70-72).
var productionAvatarBase64Pattern = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

// productionCharacterAvatars serves the four names over the durable lane's
// admitted project. It holds no path, no database and no character identity: the
// first two come from the frozen three-key context, the third from the request.
type productionCharacterAvatars struct {
	durable *productionDurableOpen

	// uploads is the upload owner. A nil seam is not a silent fallback: choose
	// answers AVATAR_UPLOAD_UNAVAILABLE, which is the truth about this process.
	uploads productionPickerUploads
}

// newProductionCharacterAvatars is the constructor the binder block wires. The
// upload owner is a separate argument because it is a separate owner: this file
// stages nothing and stores nothing outside the project it is asked about.
func newProductionCharacterAvatars(durable *productionDurableOpen,
	uploads productionPickerUploads) *productionCharacterAvatars {
	return &productionCharacterAvatars{durable: durable, uploads: uploads}
}

// Bind installs exactly the four names, one server.Bind per name, so an
// uninventoried or doubly bound name fails loudly at startup instead of becoming
// dead code.
func (a *productionCharacterAvatars) Bind(server *transport.Server) error {
	if a == nil || a.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if err := server.Bind(productionAvatarChooseName, a.chooseOperation()); err != nil {
		return err
	}
	for _, binding := range []struct {
		name  string
		arity int
		call  func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)
	}{
		{productionAvatarCommitName, 2, a.commit},
		{productionAvatarReadName, 1, a.readBatch},
		{productionAvatarRemoveName, 1, a.remove},
	} {
		if err := server.Bind(binding.name, a.operation(binding.arity, binding.call)); err != nil {
			return err
		}
	}
	return nil
}

// operation is the adapter the three fixed-arity names share: the arity check,
// the frozen three-key context, the borrow, and the project path from the lease.
// No name here takes a project path from the body at all.
func (a *productionCharacterAvatars) operation(arity int,
	call func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if ctx == nil || len(args) != arity+1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		contextValue, err := projectsession.ParseContext(args[arity])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		db, release, err := a.durable.borrowAuthorDatabase(ctx, caller, args[arity])
		if err != nil {
			// Already symbolic: a lease, ownership or closed-lane refusal.
			return nil, err
		}
		if db == nil || release == nil {
			return nil, security.NewHTTPError(http.StatusServiceUnavailable, "GO_OPERATION_UNAVAILABLE")
		}
		defer release()
		return call(ctx, caller, db, contextValue, args[:arity])
	}
}

// chooseOperation is choose's adapter. It is separate from operation because
// choose takes an OPTIONAL trailing upload id before the frozen context: one body
// argument is the desktop's shape (no bytes presented yet), two carry the staged
// upload. The context is always the last argument, whatever the length.
func (a *productionCharacterAvatars) chooseOperation() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if ctx == nil || len(args) < 2 || len(args) > 3 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		raw := args[len(args)-1]
		contextValue, err := projectsession.ParseContext(raw)
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		characterID, ok := productionAvatarCharacterID(args[0])
		if !ok {
			return productionAvatarFailure(errProductionAvatarInvalidCharacter), nil
		}
		uploadID := ""
		if len(args) == 3 {
			value, ok := args[1].(string)
			if !ok || value == "" || len(value) > 512 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			uploadID = value
		}
		db, release, err := a.durable.borrowAuthorDatabase(ctx, caller, raw)
		if err != nil {
			return nil, err
		}
		if db == nil || release == nil {
			return nil, security.NewHTTPError(http.StatusServiceUnavailable, "GO_OPERATION_UNAVAILABLE")
		}
		defer release()
		return a.choose(ctx, caller, db, contextValue, characterID, uploadID)
	}
}

// choose serves `character-avatar:choose`: verify the character, read the staged
// upload through the seam, decide the image by its magic bytes, and answer the
// preview the desktop answered. Nothing is written: choose is the preview, commit
// is the recording.
func (a *productionCharacterAvatars) choose(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, characterID, uploadID string) (any, error) {
	if uploadID == "" {
		return productionAvatarFailure(errProductionAvatarUploadRequired), nil
	}
	if a.uploads == nil {
		return productionAvatarFailure(errProductionAvatarUploadUnavailable), nil
	}
	exists, err := productionAvatarCharacterExists(ctx, db, characterID)
	if err != nil {
		return productionAvatarFailure(err), nil
	}
	if !exists {
		return productionAvatarFailure(errProductionAvatarNotFound), nil
	}
	owner, err := productionOwner(caller)
	if err != nil {
		// The transport already proved the caller; an unnamed owner is a shape
		// refusal rather than an avatar answer.
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	_, data, readErr := a.uploads.ReadUpload(owner.SessionID, owner.ClientID, uploadID)
	if readErr != nil {
		// An id staged for another session or tab is indistinguishable from one
		// this process never minted, which is the point of the seam's key.
		return productionAvatarFailure(errProductionAvatarUploadUnknown), nil
	}
	if int64(len(data)) > productionAvatarMaxInputBytes {
		return productionAvatarFailure(errProductionAvatarTooLarge), nil
	}
	if int64(len(data)) == 0 {
		return productionAvatarFailure(errProductionAvatarFormatInvalid), nil
	}
	mime, _, ok := productionAvatarImage(data)
	if !ok {
		return productionAvatarFailure(errProductionAvatarFormatInvalid), nil
	}
	return map[string]any{
		"success":   true,
		"cancelled": false,
		"image": map[string]any{
			"characterId": characterID,
			"mime":        mime,
			"base64":      base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

// commit serves `character-avatar:commit`: decode the bounded base64, decide the
// image by its magic bytes, install the file, then record the row.
func (a *productionCharacterAvatars) commit(ctx context.Context, _ transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	characterID, ok := productionAvatarCharacterID(args[0])
	if !ok {
		return productionAvatarFailure(errProductionAvatarInvalidCharacter), nil
	}
	encoded, ok := args[1].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	data, err := productionAvatarDecode(encoded)
	if err != nil {
		return productionAvatarFailure(err), nil
	}
	mime, extension, ok := productionAvatarImage(data)
	if !ok {
		return productionAvatarFailure(errProductionAvatarFormatInvalid), nil
	}
	storage, err := productionKnowledgeOpenStorage(contextValue.ProjectPath)
	if err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	defer func() { _ = storage.Close() }()
	previous, revision, err := productionAvatarCurrent(ctx, db, characterID)
	if err != nil {
		return productionAvatarFailure(err), nil
	}
	digest := productionAvatarDigest(data)
	relativePath, ok := productionAvatarRelativePath(characterID, revision, digest, extension)
	if !ok {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	bucket, err := productionAvatarOpenBucket(storage.storage, characterID, true)
	if err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	name := productionAvatarFileName(relativePath)
	writeErr := productionKnowledgeWriteAtomic(bucket, name, data)
	closeErr := bucket.Close()
	if writeErr != nil || closeErr != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	// The row is the roster's truth, so it is written only after its bytes are
	// durable, and it is the revision the row was read at plus one - a lost race
	// is a save failure, never a silent renumbering
	// (character-asset-repository.ts:85-88).
	recordErr := productionAvatarImmediate(ctx, db, func(conn *sql.Conn) error {
		exists, err := productionAvatarCharacterExists(ctx, conn, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return errProductionAvatarNotFound
		}
		current, next, err := productionAvatarCurrent(ctx, conn, characterID)
		if err != nil {
			return err
		}
		if next != revision || current != previous {
			return errProductionAvatarSaveFailed
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO character_avatar_assets(
 character_id, asset_revision, relative_path, content_hash, mime, byte_size, source_reference
) VALUES(?,?,?,?,?,?,'') ON CONFLICT(character_id) DO UPDATE SET
 asset_revision=excluded.asset_revision, relative_path=excluded.relative_path,
 content_hash=excluded.content_hash, mime=excluded.mime, byte_size=excluded.byte_size,
 source_reference=excluded.source_reference`,
			characterID, revision, relativePath, digest, mime, int64(len(data)))
		if err != nil {
			return errProductionAvatarSaveFailed
		}
		return nil
	})
	if recordErr != nil {
		// A refused commit removes the file it just installed, so the tree stays
		// exactly as the author left it.
		_ = productionAvatarRemoveFile(storage.storage, relativePath)
		return productionAvatarFailure(recordErr), nil
	}
	if err := storage.verify(); err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	// The superseded asset is unreachable from the roster now. A removal that
	// fails leaves an inert file no reader references; it never fails the commit
	// whose bytes are already durable and recorded.
	if previous != "" && previous != relativePath {
		_ = productionAvatarRemoveFile(storage.storage, previous)
	}
	return map[string]any{
		"success": true,
		"avatar": map[string]any{
			"characterId":   characterID,
			"assetRevision": revision,
			"mime":          mime,
			"base64":        base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

// readBatch serves `character-avatar:read-batch`: the roster's current avatars.
// A row whose file is missing, resized, symlinked or hash-mismatched is skipped
// exactly as the desktop's readMany skips it, so a damaged asset falls back to the
// initial avatar rather than answering bad bytes.
func (a *productionCharacterAvatars) readBatch(ctx context.Context, _ transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	raw, ok := args[0].([]any)
	if !ok || len(raw) > productionAvatarMaxBatch {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	seen := make(map[string]bool, len(raw))
	identities := make([]string, 0, len(raw))
	for _, entry := range raw {
		characterID, ok := productionAvatarCharacterID(entry)
		if !ok {
			return productionAvatarFailure(errProductionAvatarInvalidCharacter), nil
		}
		if seen[characterID] {
			continue
		}
		seen[characterID] = true
		identities = append(identities, characterID)
	}
	sort.Strings(identities)
	rows, err := productionAvatarRows(ctx, db, identities)
	if err != nil {
		return productionAvatarFailure(err), nil
	}
	if len(rows) == 0 {
		return map[string]any{"success": true, "avatars": []any{}}, nil
	}
	storage, err := productionKnowledgeOpenStorage(contextValue.ProjectPath)
	if err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	defer func() { _ = storage.Close() }()
	avatars := make([]any, 0, len(rows))
	for _, row := range rows {
		data, ok := productionAvatarReadFile(storage.storage, row)
		if !ok {
			continue
		}
		avatars = append(avatars, map[string]any{
			"characterId":   row.CharacterID,
			"assetRevision": row.Revision,
			"mime":          row.Mime,
			"base64":        base64.StdEncoding.EncodeToString(data),
		})
	}
	return map[string]any{"success": true, "avatars": avatars}, nil
}

// remove serves `character-avatar:remove`. It is idempotent in the reference's
// sense: a character with no avatar is a success that writes nothing, while a
// character that does not exist is CHARACTER_NOT_FOUND (character-asset-service.ts:
// 192-197).
func (a *productionCharacterAvatars) remove(ctx context.Context, _ transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	characterID, ok := productionAvatarCharacterID(args[0])
	if !ok {
		return productionAvatarFailure(errProductionAvatarInvalidCharacter), nil
	}
	removed := ""
	recordErr := productionAvatarImmediate(ctx, db, func(conn *sql.Conn) error {
		exists, err := productionAvatarCharacterExists(ctx, conn, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return errProductionAvatarNotFound
		}
		previous, _, err := productionAvatarCurrent(ctx, conn, characterID)
		if err != nil {
			return err
		}
		if previous == "" {
			// No avatar to drop: an idempotent success that writes nothing.
			return nil
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM character_avatar_assets WHERE character_id=?",
			characterID); err != nil {
			return errProductionAvatarSaveFailed
		}
		removed = previous
		return nil
	})
	if recordErr != nil {
		return productionAvatarFailure(recordErr), nil
	}
	if removed == "" {
		return map[string]any{"success": true}, nil
	}
	storage, err := productionKnowledgeOpenStorage(contextValue.ProjectPath)
	if err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	defer func() { _ = storage.Close() }()
	if err := productionAvatarRemoveFile(storage.storage, removed); err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	if err := storage.verify(); err != nil {
		return productionAvatarFailure(errProductionAvatarSaveFailed), nil
	}
	return map[string]any{"success": true}, nil
}

// productionAvatarFailure is this family's one and only mapping layer: one error,
// one symbolic envelope. The reference answers `{success:false, error:{code,
// message}}` for every business failure and never throws
// (character-avatar-controller.ts:28-41), so this returns the wire body, not an
// error. `message` repeats the code: the reference's localized sentences belong to
// the renderer, and this lane sends no prose.
func productionAvatarFailure(err error) map[string]any {
	code := productionAvatarCode(err)
	return map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": code},
	}
}

// productionAvatarCode maps one error to one symbolic code. It is the only
// errors.Is fan-out in this file.
func productionAvatarCode(err error) string {
	switch {
	case errors.Is(err, errProductionAvatarInvalidCharacter):
		return productionAvatarCodeInvalidCharacter
	case errors.Is(err, errProductionAvatarNotFound):
		return productionAvatarCodeNotFound
	case errors.Is(err, errProductionAvatarReadFailed):
		return productionAvatarCodeReadFailed
	case errors.Is(err, errProductionAvatarFormatInvalid):
		return productionAvatarCodeFormatInvalid
	case errors.Is(err, errProductionAvatarTooLarge):
		return productionAvatarCodeTooLarge
	case errors.Is(err, errProductionAvatarUploadRequired):
		return productionAvatarCodeUploadRequired
	case errors.Is(err, errProductionAvatarUploadUnavailable):
		return productionAvatarCodeUploadUnavailable
	case errors.Is(err, errProductionAvatarUploadUnknown):
		return productionAvatarCodeUploadUnknown
	}
	return productionAvatarCodeSaveFailed
}

// productionAvatarCharacterID is the controller's CHARACTER_ID check plus the one
// normalization every name shares.
func productionAvatarCharacterID(value any) (string, bool) {
	characterID, ok := value.(string)
	if !ok || !productionAvatarCharacterPattern.MatchString(characterID) {
		return "", false
	}
	return characterID, true
}

// productionAvatarDecode is the reference's decode half
// (character-asset-service.ts:128-135): the shape and the pre-decode bound first,
// then the decoded bound, then a non-empty payload. An empty or malformed base64
// is IMAGE_FORMAT_INVALID, never a read failure.
func productionAvatarDecode(encoded string) ([]byte, error) {
	if encoded == "" || len(encoded)%4 != 0 || !productionAvatarBase64Pattern.MatchString(encoded) {
		return nil, errProductionAvatarFormatInvalid
	}
	if len(encoded) > productionAvatarMaxBase64Bytes {
		return nil, errProductionAvatarTooLarge
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errProductionAvatarFormatInvalid
	}
	if len(data) == 0 {
		return nil, errProductionAvatarFormatInvalid
	}
	if len(data) > productionAvatarMaxInputBytes {
		return nil, errProductionAvatarTooLarge
	}
	return data, nil
}

// productionAvatarImage is detectAvatarExtension and avatarMime
// (electron/services/avatar-image.ts:6-24) in one decision: the magic bytes
// alone, never a name, never a declared type. It answers the mime the schema's
// CHECK accepts and the file extension the desktop would derive (`jpeg` -> `jpg`).
func productionAvatarImage(data []byte) (string, string, bool) {
	if len(data) >= 8 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4e && data[3] == 0x47 &&
		data[4] == 0x0d && data[5] == 0x0a && data[6] == 0x1a && data[7] == 0x0a {
		return productionAvatarMimePNG, "png", true
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return productionAvatarMimeJPEG, "jpg", true
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return productionAvatarMimeWebP, "webp", true
	}
	return "", "", false
}

// productionAvatarDigest is createHash('sha256').update(bytes).digest('hex') over
// the image's own bytes (character-asset-service.ts:139).
func productionAvatarDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// productionAvatarRelativePath is the desktop's asset path
// (character-asset-service.ts:141): avatars/<sha256hex(characterId)>/
// <assetRevision>-<contentHash>.<ext>. Nothing in it comes from a request, so it
// cannot address anything else.
func productionAvatarRelativePath(characterID string, revision int64, digest, extension string) (string, bool) {
	if revision < 1 || !productionAvatarDigestValid(digest) || !productionAvatarExtensionValid(extension) {
		return "", false
	}
	path := productionAvatarDirectory + "/" + productionAvatarBucket(characterID) + "/" +
		strconv.FormatInt(revision, 10) + "-" + digest + "." + extension
	if len(path) > 200 {
		return "", false
	}
	return path, true
}

// productionAvatarFileName is the last component of a relative asset path.
func productionAvatarFileName(relativePath string) string {
	if index := strings.LastIndexByte(relativePath, '/'); index >= 0 {
		return relativePath[index+1:]
	}
	return relativePath
}

// productionAvatarBucket is the per-character directory names's digest, exactly
// the reference's `avatars/${hash(characterId)}`.
func productionAvatarBucket(characterID string) string {
	digest := sha256.Sum256([]byte(characterID))
	return hex.EncodeToString(digest[:])
}

// productionAvatarDigestValid is 64 lowercase hex, the reference's content hash
// and the schema's CHECK(length=64).
func productionAvatarDigestValid(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, digit := range digest {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}

// productionAvatarExtensionValid is the three extensions the desktop derives.
func productionAvatarExtensionValid(extension string) bool {
	return extension == "png" || extension == "jpg" || extension == "webp"
}

// productionAvatarRelativePathValid bounds a relative path read back out of the
// database: structural only, since os.Root is what confines it.
func productionAvatarRelativePathValid(relative string) bool {
	if relative == "" || len(relative) > 200 || !strings.HasPrefix(relative, productionAvatarDirectory+"/") {
		return false
	}
	if strings.ContainsAny(relative, "\\\x00") || strings.HasSuffix(relative, "/") {
		return false
	}
	for _, part := range strings.Split(relative, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// productionAvatarRecord is one read row of the roster's current assets.
type productionAvatarRecord struct {
	CharacterID string
	Revision    int64
	Relative    string
	Digest      string
	Mime        string
	Size        int64
}

// productionAvatarRows reads the rows for the given identities. The IN list is
// built from validated identities only, and its length is the batch bound.
func productionAvatarRows(ctx context.Context, db *sql.DB,
	identities []string) ([]productionAvatarRecord, error) {
	if len(identities) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(identities))
	for position := range identities {
		placeholders[position] = "?"
	}
	query := `SELECT character_id, asset_revision, relative_path, content_hash, mime, byte_size
 FROM character_avatar_assets WHERE character_id IN (` + strings.Join(placeholders, ",") + `)
 ORDER BY character_id`
	arguments := make([]any, len(identities))
	for position, identity := range identities {
		arguments[position] = identity
	}
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, errProductionAvatarSaveFailed
	}
	defer func() { _ = rows.Close() }()
	records := make([]productionAvatarRecord, 0, len(identities))
	for rows.Next() {
		var record productionAvatarRecord
		if err := rows.Scan(&record.CharacterID, &record.Revision, &record.Relative, &record.Digest,
			&record.Mime, &record.Size); err != nil {
			return nil, errProductionAvatarSaveFailed
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, errProductionAvatarSaveFailed
	}
	return records, nil
}

// productionAvatarCurrent reads the character's current asset and the revision a
// commit must record. It runs on whichever handle the caller holds, so the commit's
// transaction reads the row it is about to replace in its own snapshot.
func productionAvatarCurrent(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, characterID string) (string, int64, error) {
	var relative string
	var revision int64
	err := queryer.QueryRowContext(ctx,
		"SELECT relative_path, asset_revision FROM character_avatar_assets WHERE character_id=?",
		characterID).Scan(&relative, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 1, nil
	}
	if err != nil {
		return "", 0, errProductionAvatarSaveFailed
	}
	return relative, revision + 1, nil
}

// productionAvatarCharacterExists is the reference's `SELECT 1 FROM characters
// WHERE character_id=? AND retired=0` (character-asset-repository.ts:56-64). It
// runs on whichever handle the caller holds, so the commit's transaction sees the
// same snapshot as its row read.
func productionAvatarCharacterExists(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, characterID string) (bool, error) {
	var found int
	err := queryer.QueryRowContext(ctx,
		"SELECT 1 FROM characters WHERE character_id=? AND retired=0 LIMIT 1", characterID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errProductionAvatarSaveFailed
	}
	return true, nil
}

// productionAvatarReadFile reads and re-verifies one stored asset. Every check the
// desktop's readMany makes is here: a regular file, no symlink, the recorded size,
// the recorded digest, and bytes that are still an image of the recorded mime.
func productionAvatarReadFile(root *os.Root, record productionAvatarRecord) ([]byte, bool) {
	if root == nil || !productionAvatarRelativePathValid(record.Relative) ||
		!productionAvatarDigestValid(record.Digest) || !productionAvatarMimeValid(record.Mime) ||
		record.Size <= 0 || record.Size > productionAvatarMaxInputBytes ||
		record.Revision < 1 {
		return nil, false
	}
	file, err := root.OpenFile(record.Relative, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	info, statErr := file.Stat()
	if statErr != nil || info == nil || !info.Mode().IsRegular() || info.Size() != record.Size {
		_ = file.Close()
		return nil, false
	}
	data, readErr := io.ReadAll(io.LimitReader(file, productionAvatarMaxInputBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(data)) != record.Size {
		return nil, false
	}
	if productionAvatarDigest(data) != record.Digest {
		return nil, false
	}
	mime, _, ok := productionAvatarImage(data)
	if !ok || mime != record.Mime {
		return nil, false
	}
	return data, true
}

// productionAvatarMimeValid is the schema's three-value CHECK.
func productionAvatarMimeValid(mime string) bool {
	return mime == productionAvatarMimePNG || mime == productionAvatarMimeJPEG || mime == productionAvatarMimeWebP
}

// productionAvatarOpenBucket opens avatars/<sha256hex(characterId)> as a child
// root, creating both levels when the writer asks. The bucket is opened as a
// directory root, so a write can only ever land inside it.
func productionAvatarOpenBucket(storage *os.Root, characterID string, create bool) (*os.Root, error) {
	if storage == nil {
		return nil, errProductionAvatarSaveFailed
	}
	avatars, err := productionAvatarChild(storage, productionAvatarDirectory, create)
	if err != nil || avatars == nil {
		return nil, errProductionAvatarSaveFailed
	}
	bucket, err := productionAvatarChild(avatars, productionAvatarBucket(characterID), create)
	closeErr := avatars.Close()
	if err != nil || closeErr != nil || bucket == nil {
		if bucket != nil {
			_ = bucket.Close()
		}
		return nil, errProductionAvatarSaveFailed
	}
	return bucket, nil
}

// productionAvatarChild opens (and optionally creates) one directory level as a
// child root, refusing anything that is not a real directory.
func productionAvatarChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if parent == nil || name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return nil, errProductionAvatarSaveFailed
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, errProductionAvatarSaveFailed
		}
		if mkdirErr := parent.Mkdir(name, 0700); mkdirErr != nil {
			return nil, errProductionAvatarSaveFailed
		}
		info, err = parent.Lstat(name)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errProductionAvatarSaveFailed
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, errProductionAvatarSaveFailed
	}
	return child, nil
}

// productionAvatarRemoveFile drops one stored asset. An already absent file is a
// success: remove is idempotent, and so is reclaiming a superseded asset.
func productionAvatarRemoveFile(storage *os.Root, relativePath string) error {
	if storage == nil || !productionAvatarRelativePathValid(relativePath) {
		return errProductionAvatarSaveFailed
	}
	info, err := storage.Lstat(relativePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || info == nil || !info.Mode().IsRegular() {
		return errProductionAvatarSaveFailed
	}
	if err := storage.Remove(relativePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errProductionAvatarSaveFailed
	}
	return nil
}

// productionAvatarImmediate runs body inside one BEGIN IMMEDIATE transaction on
// one reserved connection, the shape internal/store uses for every author write
// (internal/store/drafts.go:226-262). The row read, its checks and its write
// therefore see one snapshot, and a request canceled mid-write still rolls back.
func productionAvatarImmediate(ctx context.Context, db *sql.DB, body func(*sql.Conn) error) (retErr error) {
	if db == nil || body == nil {
		return errProductionAvatarSaveFailed
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return errProductionAvatarSaveFailed
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return errProductionAvatarSaveFailed
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errProductionAvatarSaveFailed
			// A trigger may already have rolled back, but an uncertain rollback
			// discards the connection rather than leaking transaction state.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := body(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return errProductionAvatarSaveFailed
	}
	committed = true
	return nil
}
