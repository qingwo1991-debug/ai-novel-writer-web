# Isolated native Lance compatibility spike

Not a production adapter. `novel_lance` is a Rust static library linked **in-process** by Go/CGO under the explicit `lance_native && cgo` build tag. The normal application and standard Go build do not include it. No Node/Python subprocess or server is used by this native executable. Node 0.22.3 is a separate GitHub-only synthetic oracle.

## Qualified synthetic result

[GitHub41420077722](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41420077722), source `bc14231b`: Rust locked build, Go vet,5native tests0fail0skip, PATH=/nonexistent real native driver and original Node verifier all passed. Six tables/14rows, full schemas/rows, empty stream table, latestv2→freshv1, and source/copy file contents/membership including registry/copies unchanged. Measurements: Rust build5m25s/maxRSS1619064KiB, target3.0G/staticlib841M/strippedGo215M; tiny fixture0.10s/maxRSS109712KiB. ELF dynamically needs libm/libgcc_s/libc/loader, not a fully-static executable. No author workload performance claim. Persisted bounded evidence: `web/evidence/go-lance-native-round15.json`.

## Version and build policy

- Exact Rust LanceDB `=0.22.3`, matching npm `@lancedb/lancedb@0.22.3` release; underlying Lance family0.39.0.
- Direct Arrow array/schema/ipc `=56.2.0`; Rust1.90.0.
- GitHub bootstrap used immutable upstream release Cargo.lock, then resolved the shim root. Committed lock SHA256 `a6252b1526248b36ff4168826cf3d91b7b43dc54b69ac600854d8d5bac705365`:591 packages; all shared registry checksums and Lance/Arrow/DataFusion family versions match upstream, including upstream's deliberate DataFusion physical-expr-adapter50.0.0 exception. New package/version pairs are only novel-lance0.1.0 and published lancedb0.22.3; upstream release workspace lock still named its unpublished root0.22.3-beta.5. Published0.22.3 checksum separately matches crates.io version metadata. Builds use `--locked`; this audit is dependency identity evidence, not a successful native build.
- Compilation, rustfmt/gofmt, tests and dependency installs **only on GitHub Actions**, never the deployment machine. Separate `.github/workflows/go-lance-native.yml` avoids making the lightweight Go regression job build Lance on every change. This job never deploys/publishes an image.

## Distribution follow-up

GitHub `cargo metadata` contains dependency/license declarations but is not a final linked-artifact SBOM or legal approval. Among591 resolved packages, option-ext0.2.0 declares MPL-2.0; r-efi5.3.0 offers MIT OR Apache-2.0 OR LGPL-2.1-or-later. Native distribution must inventory actually linked Rust/C libraries, choose permitted alternatives where applicable, preserve notices and satisfy relevant source obligations before release. This unpublished shim has no independent distribution license declaration. No artifact is deployed by this spike.

## Narrow ABI

See `include/novel_lance.h`: synchronous one-shot `nv_lance_export`, `nv_lance_import`, `nv_lance_free`. Results and diagnostics are Rust-owned boxed byte buffers; Go copies results and frees through Rust, never retains Go pointers in Rust. `catch_unwind` does not contain abort/OOM/UB. Context is checked before/after each call, not cancellable in the middle of a native query; no early goroutine return/free races.

Export opens an already existing private physical copy, enumerates allowed tables, reads current version, checks out that exact version, scans all rows, and writes schema-bearing Arrow **stream** IPC even for empty tables. Import validates stream syntax/row counts, creates only in an existing empty private staging root, and returns fresh actual versions. It does not restore old history, indices or registries. Each IPC max64MiB, manifest1MiB,32tables, retained input IPC128MiB total; malformed input rejected, no silent truncation. Decoding and individual Lance/Arrow batches still need additional hostile-input memory controls before a production adapter.

Roots must be canonical absolute physical existing owner-private directories, disjoint, with no symlink ancestors or URI syntax. Copy tree rejects symlinks, hardlinked/special files and oversized/deep membership. These checks do **not** establish author-vs-copy provenance, safe arbitrary manifest references, or exclusion against concurrent same-user writers. Caller must supply only trusted synthetic private copies. Failed output/staging may be partially populated; discard attempt-owned staging, never publish it.

## Oracle evidence and limits

`contract/lance-native-contract.mjs` uses fixed actual npm0.22.3/Arrow18.1.0: documents, canonical chunks, active2D/inactive3D/building2D embedding generations plus a schema-bearing empty table, nullable chapter fields, Unicode and Float32 values, and an active table second version. It checks complete schema/field-order/nullability/list child and rows from original stream vs native streams vs original Node reopening native-created Lance tables, as well as original/source-copy complete file SHA256/membership before/after reads. Latest version is pinned; no historical override checkout claim.

Real original persistence changes vector-list child nullability to true both for createTable and createEmptyTable despite the application's supplied nonnullable child Field. The oracle retains the actual persisted schema. Original toArrow may lose schema for no batches; for a proven zero-row table the oracle uses its actual persisted schema to construct schema-only Arrow stream.

Canonical registry and knowledge-copy bytes are kept unchanged in source and copied storage; this spike does not migrate them or prove SQL/document/copy consistency in application workflows. No exact JS logicalHash/localeCompare parity, search/FTS/index compatibility, native query deadlines, full-version history, Windows/macOS packaging, production writer lifecycle or complete KB operations are claimed. See `web/GO-LANCE-NATIVE-SPIKE.md` and `web/GO-CI-ACCEPTANCE.md` for actual remote outcomes; source existence is not a passing result.
