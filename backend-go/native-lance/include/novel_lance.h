#ifndef NOVEL_LANCE_H
#define NOVEL_LANCE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Rust-owned, not NUL-terminated. Never use C.free. */
typedef struct NvBytes {
    uint8_t *ptr;
    size_t len;
} NvBytes;

/* Stable return statuses; err contains only the corresponding constant token. */
enum {
    NV_LANCE_OK = 0,
    NV_LANCE_INPUT = 1,
    NV_LANCE_PATH = 2,
    NV_LANCE_LIMIT = 3,
    NV_LANCE_DATABASE = 4,
    NV_LANCE_IPC = 5,
    NV_LANCE_IO = 6,
    NV_LANCE_RUNTIME = 7,
    NV_LANCE_PANIC = 8
};

/*
 * Synchronous, stateless, in-process operations. No input pointers are retained.
 * All pointer/length pairs must denote valid readable memory for the full call.
 * result/err must be distinct valid writable NvBytes, initially empty (free any
 * previous contents first). Both are zeroed on entry. On success result is UTF-8
 * JSON {"tables":[{"name":...,"version":...,"rows":...,"file":...}]}; err is
 * empty. On failure result is empty and err is a sanitized constant token.
 * Free BOTH buffers on every return path. A caught unwind returns NV_LANCE_PANIC;
 * invalid foreign pointers, allocation aborts and process termination cannot be
 * recovered by catch_unwind.
 *
 * Paths must be existing, absolute canonical physical Unix directories owned by
 * the effective user with no group/other permissions. No URI, NUL, symlink path
 * components or overlapping input/output roots. Caller must exclusively control
 * these private directories, exclude concurrent writers and supply ONLY trusted
 * synthetic fixture copies, NEVER an author's original database. This ABI cannot
 * authenticate copy provenance or sandbox references inside Lance manifests.
 *
 * Export path is the LanceDB root itself (e.g. copy/lancedb); outdir must be EMPTY.
 * Every listed name must be chunks, documents or chunks__space_[0-9]+, at most 32
 * tables. Opens existing tables only, pins each latest version with checkout and
 * full-scans all fields into exclusive mode-0600 <name>.arrows IPC STREAM files.
 * Each IPC file is limited to 64 MiB, including schema and stream terminator.
 * No manifest file is written. Version selection is per-table, not a cross-table
 * transaction. No mutation API is used on the export database; caller must still
 * compare its before/after filesystem manifest as an independent side-effect gate.
 */
int32_t nv_lance_export(const uint8_t *path, size_t pathlen,
                        const uint8_t *outdir, size_t outlen,
                        NvBytes *result, NvBytes *err);

/*
 * staging is an existing EMPTY private LanceDB root; inputdir holds IPC STREAMS.
 * manifest is at most 1 MiB and has exactly the same JSON shape as export output;
 * tables are unique, filenames exactly <name>.arrows, version > 0. Creates each
 * table in Create (never overwrite) mode directly from its IPC stream/schema,
 * including zero-row schema-only streams. Checks declared rows before any write
 * and scans created tables to verify rows and schema. Returns actual NEW versions:
 * source version numbers, history, indices and physical layout are NOT replayed.
 * No external embedding registry/functions are supplied. No format compatibility
 * or migration safety is asserted without the separate oracle/CI checks.
 *
 * Failure may leave partial output files/tables. Caller must discard the entire
 * private outdir/staging on failure; do not retry using it and never promote it.
 */
int32_t nv_lance_import(const uint8_t *staging, size_t staginglen,
                        const uint8_t *inputdir, size_t inputlen,
                        const uint8_t *manifest, size_t manifestlen,
                        NvBytes *result, NvBytes *err);

/* Catches unwinds; accepts NULL and empty buffers, zeroes *bytes before freeing.
 * Only pass an unmodified buffer returned by this library, once; not a copied
 * alias, arbitrary allocation, or a buffer currently being used by another call.
 */
void nv_lance_free(NvBytes *bytes);

#ifdef __cplusplus
}
#endif
#endif
