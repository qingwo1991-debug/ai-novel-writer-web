# Isolated native Lance compatibility spike

Not a production adapter. `novel_lance` is a Rust static library linked **in-process** by Go/CGO under the explicit `lance_native && cgo` build tag. The normal application and standard Go build do not include it. No Node/Python subprocess or server is used by this native executable. Node 0.22.3 is a separate GitHub-only synthetic oracle.

## Version and build policy

- Exact Rust LanceDB `=0.22.3`, matching npm `@lancedb/lancedb@0.22.3` release; underlying Lance family0.39.0.
- Direct Arrow array/schema/ipc `=56.2.0`; Rust1.90.0.
- GitHub bootstrap starts from the immutable upstream release Cargo.lock, resolves shim root, emits the resulting lock for audit/commit. A missing committed lock means reproducibility qualification remains incomplete, not an implicit latest dependency approval.
- Compilation, rustfmt/gofmt, tests and dependency installs **only on GitHub Actions**, never the deployment machine. Separate `.github/workflows/go-lance-native.yml` avoids making the lightweight Go regression job build Lance on every change. This job never deploys/publishes an image.

## Narrow ABI

See `include/novel_lance.h`: synchronous one-shot `nv_lance_export`, `nv_lance_import`, `nv_lance_free`. Results and diagnostics are Rust-owned boxed byte buffers; Go copies results and frees through Rust, never retains Go pointers in Rust. `catch_unwind` does not contain abort/OOM/UB. Context is checked before/after each call, not cancellable in the middle of a native query; no early goroutine return/free races.

Export opens an already existing private physical copy, enumerates allowed tables, reads current version, checks out that exact version, scans all rows, and writes schema-bearing Arrow **stream** IPC even for empty tables. Import validates stream syntax/row counts, creates only in an existing empty private staging root, and returns fresh actual versions. It does not restore old history, indices or registries. Each IPC max64MiB, manifest1MiB,32tables, retained input IPC128MiB total; malformed input rejected, no silent truncation. Decoding and individual Lance/Arrow batches still need additional hostile-input memory controls before a production adapter.

Roots must be canonical absolute physical existing owner-private directories, disjoint, with no symlink ancestors or URI syntax. Copy tree rejects symlinks, hardlinked/special files and oversized/deep membership. These checks do **not** establish author-vs-copy provenance, safe arbitrary manifest references, or exclusion against concurrent same-user writers. Caller must supply only trusted synthetic private copies. Failed output/staging may be partially populated; discard attempt-owned staging, never publish it.

## Oracle evidence and limits

`contract/lance-native-contract.mjs` uses fixed actual npm0.22.3/Arrow18.1.0: documents, canonical chunks, active2D/inactive3D/building2D embedding generations plus a schema-bearing empty table, nullable chapter fields, Unicode and Float32 values, and an active table second version. It checks complete schema/field-order/nullability/list child and rows from original stream vs native streams vs original Node reopening native-created Lance tables, as well as original/source-copy complete file SHA256/membership before/after reads. Latest version is pinned; no historical override checkout claim.

Real original persistence changes vector-list child nullability to true both for createTable and createEmptyTable despite the application's supplied nonnullable child Field. The oracle retains the actual persisted schema. Original toArrow may lose schema for no batches; for a proven zero-row table the oracle uses its actual persisted schema to construct schema-only Arrow stream.

Canonical registry and knowledge-copy bytes are kept unchanged in source and copied storage; this spike does not migrate them or prove SQL/document/copy consistency in application workflows. No exact JS logicalHash/localeCompare parity, search/FTS/index compatibility, native query deadlines, full-version history, Windows/macOS packaging, production writer lifecycle or complete KB operations are claimed. See `web/GO-LANCE-NATIVE-SPIKE.md` and `web/GO-CI-ACCEPTANCE.md` for actual remote outcomes; source existence is not a passing result.
