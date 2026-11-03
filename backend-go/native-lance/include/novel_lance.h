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
 * Also writes one exclusive mode-0600 domain.json from those SAME batches, even
 * for an empty database. The returned manifest and IPC formats are unchanged;
 * import does not read the sidecar. Domain JSON is {"tables":[{"name":string,
 * "version":u64,"metadata":bool,"schema":[FIELD...],"records":[OBJECT...]}]}.
 * FIELD is {"name":string,"type":string,"nullable":bool,"listSize":int,
 * "metadata":bool,"children":[FIELD...]}; scalar listSize is 0. Metadata booleans
 * report presence only. Records include every schema field, preserving JSON null.
 * Supported Arrow types: utf8, int32, float32, float64 and fixed_size_list with
 * recursively supported children. Other types and nonfinite numbers fail IPC.
 * Float32 values are widened to f64 before JSON serialization. Object key order,
 * negative-zero round trips and canonical logical hashes are NOT guaranteed.
 * Limits across ALL tables: 10,000 rows, 200,000 cells (lists and child scalars
 * count), 32 MiB UTF-8 string bytes (including repeated record keys), 128 schema
 * fields including children and maximum recursive depth 16. Serialized domain
 * JSON is capped at 64 MiB while encoding. These bounds are NOT a process-RSS or
 * underlying Arrow batch allocation bound; this is a narrow synthetic domain,
 * not general Arrow export compatibility. No external registry is native input.
 * No manifest file is written. Version selection is per-table, not a cross-table
 * transaction. No mutation API is used on the export database; caller must still
 * compare its before/after filesystem manifest as an independent side-effect gate.
 */
int32_t nv_lance_export(const uint8_t *path, size_t pathlen,
                        const uint8_t *outdir, size_t outlen,
                        NvBytes *result, NvBytes *err);

/*
 * Read-only bounded flat vector query on a trusted private Lance copy. The
 * request is UTF-8 JSON with exact fields table, version, generation,
 * dimension, metric (l2/cosine/dot), modelFingerprint, topK and vector.
 * table must equal chunks__space_<generation>; version must be current.
 * modelFingerprint is echoed as a caller-validated registry binding, not
 * independently authenticated by Lance. The caller must validate registry,
 * source/currentness and document-copy state before using these rows.
 * No index is used; results are limited to 100 and serialized JSON is capped
 * at 4 MiB. result is {table,version,generation,dimension,metric,
 * modelFingerprint,records:[...]} with finite _distance per record. The same
 * private copy/path/concurrent-writer requirements as export apply.
 */
int32_t nv_lance_vector_query(const uint8_t *path, size_t pathlen,
                              const uint8_t *request, size_t requestlen,
                              NvBytes *result, NvBytes *err);

/*
 * staging is an existing EMPTY private LanceDB root; inputdir holds IPC STREAMS.
 * Input IPC is at most 64 MiB per table and 128 MiB retained in aggregate; this
 * does not bound decoded Arrow allocation for hostile/compressed input. Only
 * trusted synthetic inputs are supported. The manifest is at most 1 MiB and
 * has exactly the same JSON shape as export output;
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
