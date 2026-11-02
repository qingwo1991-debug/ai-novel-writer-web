# Exact-image isolated HTTP boundary acceptance

## Scope

- Runtime: published GHCR digest `sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37`, actual image ID `sha256:00010f8df6050c89d430eb35222bbc5610a4e8c45dbfb4bca9aa25273d06d977`.
- Disposable gateway on copied `.native-test-data`, host `127.0.0.1:16081`; production `6081` and author volume were not used by this script.
- `NOVEL_ALLOWED_ENDPOINTS` was explicitly empty. No provider, paid-generation, shell/tool-execution or genuine MCP server operation was sent.
- This is direct HTTP testing of the trusted-proxy contract, **not rendered-browser or nginx password acceptance**. Test principals are supplied only at the isolated loopback gateway.

## Result

**32 checks passed**, zero paid dispatches; private report: `.native-test-data/http-boundary/2026-11-02T00-33-38-352Z/report.json`.

| Boundary | Verified behavior |
| --- | --- |
| Native health | Node / non-Electron |
| Authentication/session | Missing identity rejected; cookie HttpOnly and SameSite Strict |
| CSRF/Origin | Missing token, wrong Origin and cross-site requests rejected |
| JSON/arguments | Wrong content type, malformed JSON, non-array arguments, prototype keys and excessive depth rejected |
| Exposure policy | Desktop close, private library RPC, MCP config path and unknown operation rejected |
| Ordinary-author MCP | Configuration load and server connect rejected before any subprocess |
| Host-managed config | Unknown setting and browser-supplied deployment proxy replacement rejected |
| Managed library | Outside-library project path rejected |
| Provider policy | Empty allowlist denies test-connection request before provider dispatch |
| Capabilities | Unknown dialog/download rejected; foreign-origin SSE rejected before stream connection |
| Staged uploads | Owned folder creation and valid nested Unicode file accepted |
| Upload ownership/path | Foreign session denied; parent traversal, absolute, backslash, dot-component and NUL paths rejected |

The test creates only one synthetic staged upload and no project or model changes. Reports contain check names/counts, not cookies, CSRF values, provider credentials or author prose.

## Reproduction and limits

```sh
# First verify the disposable gateway identity, copied mount, and empty provider policy.
NOVEL_BOUNDARY_IMAGE=sha256:00010f8df6050c89d430eb35222bbc5610a4e8c45dbfb4bca9aa25273d06d977 \
  node web/http-boundary.integration.mjs
```

The script refuses URLs other than its fixed disposable loopback entry and writes reports only under ignored `.native-test-data`. It does not start services, alter firewall/nginx, log in to a registry, change production files, or reset credentials. This is bounded negative testing, not exhaustive fuzzing, DNS/egress isolation, multi-tenant certification, arbitrary subprocess sandboxing, or a substitute for real browser workflows.
