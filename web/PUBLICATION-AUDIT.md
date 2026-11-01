# Native-Web source publication check

This derivative preserves the upstream GPL-3.0 license and baseline history. Before its initial push to the separate private GitHub repository, a read-only check covered the tracked and non-ignored working-tree publication candidates.

## Checked snapshot

- 2,231 existing regular files, approximately 176.7 MB; pending-deleted upstream workflow paths were not treated as source files.
- Actual credentials from the production and isolated-test active model registries were compared privately against candidate files, including literal, JSON-escaped, Base64 and URL-encoded forms. No matches were found. No credential values are included here.
- Pattern checks found no detected real private key, GitHub token, AWS access-key ID, JWT, long model key, or populated application session cookie. Reviewed assignment/URL matches were test fixtures, dry-run examples, environment references or source declarations.
- The 86 then-new source candidates contained no unexpected live database, archive, HAR, certificate, key, downloaded author manuscript or binary test output.
- Existing SQLite/Lance/gzip migration fixtures were already upstream-tracked synthetic test assets, not copies of production author data.
- Git exclusions cover `.native-test-data/`, environment files, `docker/backups/`, Python bytecode and generated bundles. Docker exclusions also reject local environment files and private acceptance data. Explicit Docker source-copy paths do not include a server data volume.

## Limits

This is a bounded working-tree check, **not** a promise of arbitrary secret detection or a complete upstream Git-history scan. Existing upstream image/video pixels were not OCR-reviewed. Unknown credentials or arbitrary encodings can evade pattern checks. Nonsecret example LAN addresses and host paths remain in deployment configuration/documentation and must be adapted for other installations.

Production author data, archives, model registries, cookies, screenshots and raw browser test reports remain outside the source publication. Private repository visibility does not replace credential hygiene. GitHub Actions must never receive author volumes or model API credentials; image publication uses its repository-scoped `GITHUB_TOKEN`.
