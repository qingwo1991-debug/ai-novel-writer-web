// Package librarypath is the restricted path and capability foundation every
// browser-ised file operation shares. It replaces the one authority a browser
// cannot have - a native picker that answers with a host path - with three
// server-side ones: an allowlist of roots, a bounded listing of those roots, and
// an opaque, caller-bound token that names exactly one resolved path inside them.
//
// # Why a browser needs this at all
//
// The desktop renderer never sends an absolute path for the `fs:grant-*` family:
// it carries only a grantId the main process minted after a native dialog
// (src/shared/ipc-channels.ts:488-502, electron/controllers/
// external-file-grant-controller.ts:114-180). In web mode the same renderer calls
// the same channels (src/services/ipc-client.ts:38-45 classifies them as
// capability channels and sends them to the server). A browser has no picker,
// so the authority has to come from configuration instead: only the admitted
// project root and the configured library root, and their descendants, are
// addressable. Everything else - an absolute path, `..`, a separator smuggled
// through URL decoding, a symlink planted inside a root that points out of it -
// is refused by a symbolic name before any filesystem call.
//
// # A grant is not a capability to write author data
//
// This is the one rule that must not be "simplified" later. A grant authorises a
// PATH, nothing else. It does not authorise a write into author data, and it
// never stands in for the author database's single-writer lease. Writing author
// data goes through the operation that owns that data, under the lease that
// operation already holds; a target inside a project tree is therefore refused
// for write and mkdir (ErrAuthorData) and the caller is expected to use the
// owning operation instead. What a browser grant CAN write is an output: a file
// below a root that is not a project tree, which is the export lane the desktop
// served through the user's chosen directory. Reads are not restricted the same
// way, because reading a manuscript to import it writes nothing.
//
// # Shape
//
//   - Roots (roots.go) is the allowlist for one request: configured roots, keyed
//     by a symbolic id, plus Resolve, which turns a root id and a relative path
//     into a Target - the resolved identity of a path - or a refusal.
//   - ListDirectory (listing.go) answers depth 1, an entry cap and no file
//     contents, so a browser can draw a chooser without opening anything.
//   - Registry (grants.go) mints, redeems, expires and revokes tokens. It is the
//     same shape as internal/reviewrevprep/store.go: one mutex, a per-process
//     key, an HMAC over each entry's own fields, expiry pruning on every
//     mutation, and a caller revision epoch so a revoke racing a mint fails
//     closed.
//   - The Target methods in io.go are the only place this package touches the
//     filesystem, and they re-derive nothing from a browser string: they operate
//     on a target the allowlist already resolved, through a pinned os.Root.
//
// # What this package deliberately does not do
//
// It opens no database, admits no project, and does not know what a lease is. It
// never accepts an absolute path from a caller: Roots.Resolve takes a root id the
// server chose plus a relative path, and answers a Target whose Path is
// recomputed from the allowlisted root - a caller's string is never echoed back
// as a path. It also does not decide which roots exist or who the caller is; the
// adapter builds the allowlist from the live lease and passes the cookie
// session and tab identity in as a Binding.
package librarypath
