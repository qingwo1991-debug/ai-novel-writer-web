# Native credential-boundary integration acceptance

## Result and immutable target

**PASS**, 2026-11-02. The parent explicitly declared the backend image READY before execution.

- Final453 image: `sha256:8730c6b5ddd51578a7a633cd6bd6d7905eb68ad2d9bbf554977b56d1ccfd5e02`
- Successful final-image run: `mvyghykb-efc8312c`; managed job `bash-456`; exit code **0**.
- Runner: `web/credential-boundary.integration.mjs`; uses no sanitizer imports or new dependencies.
- **33** raw/decoded HTTP JSON responses audited for the exact generated credential.
- **7** SSE events audited: one native dialog event, **3 live** generation events, **3 replayed** generation events.
- **6** genuine HTTP fake-provider dispatches, all receiving the exact original Bearer credential.
- No private fixture, API key, control token, cookie, or CSRF value is reproduced in this report.

This final-image result was obtained by rerunning the exact existing canary after explicit Final453 READY and completion of the parent's planned test-gateway replacement. The earlier image `sha256:c0234f4769ea7d1b361933aa81eb35886a274055179db3bd6ddd330b3d759ed5` also passed as run `mvyfvnni-0ff932e6` / `bash-439` with the same counts. The final rerun independently executes the newer backend lifecycle code; it is not inferred from unchanged sanitizer source or a mutable tag.

## Isolation and deployment conditions

The runner created only uniquely named `novel-native-canary-*` containers/network and a tiny fresh bind-volume below ignored `.native-test-data/`. It did not copy production data or use real credentials. Both containers used the pinned image ID with `--pull=never`, read-only root filesystems, `--cap-drop=ALL`, `no-new-privileges`, bounded memory/PIDs, and a `/tmp` tmpfs. The gateway used its production entrypoint and lifetime data-volume flock.

A new Docker `--internal` network contained only the own gateway and trusted fake provider. Neither container published ports. The runner used the gateway's private bridge HTTP address, with that exact same Origin in deployment configuration. `NOVEL_ALLOWED_ENDPOINTS` contained only the exact fake-provider origin; proxy variables were explicitly cleared. No external/paid model calls were made.

Browser-equivalent requests used the trusted test `X-Novel-User` header, bootstrap-issued HttpOnly cookie, CSRF token, client ID, and same Origin. This intentionally tests the native gateway boundary, not reverse-proxy authentication.

## Exercised behavior

1. Fresh native bootstrap/list proved no preexisting models. Four models were saved through HTTP; a blank-key save restored the private stored credential.
2. The own gateway was restarted. Execution-lease generation preceded public list/test calls, exercising eager startup registration of stored credentials.
3. Public model projection blanked `apiKey`, preserved `hasCredential: true`, and redacted the same credential embedded in an ordinary display-name value.
4. Successful blank-key test-connection proved credential restoration remained functional. A malicious provider HTTP 400 body intentionally contained the key in error and ordinary payload values; the connection response redacted them.
5. Nonstream generation redacted successful content and a malicious `httpFailure.body`; the fake provider actually received both requests.
6. A tiny project was created through native browser-dialog completion and directory grant, then opened through the native project/session lifecycle.
7. Lease-bound `llm:generate-stream` emitted a canary-bearing content chunk and terminal full text. Live gateway SSE redacted both. A separate provider-error stream emitted the existing fixed `LLM_REQUEST_FAILED` event, not the provider body.
8. Reconnection with `Last-Event-ID` replayed the three generation chunk/done/error events. Replayed public events exactly matched sanitized live events.
9. HTTP LLM-history responses redacted ordinary `modelName` and `purpose` payload values containing the key.
10. Wrong-Origin, missing-CSRF, and non-allowlisted endpoint requests were rejected with the expected fixed codes before provider dispatch.

## Negative evidence against a rewritten or vacuous test

- Saved private model JSON retained the exact original key and unredacted display-name value; file mode was **0600**.
- After all public projections and generation operations, those private model JSON bytes were unchanged.
- The trusted fake provider independently checked exact Authorization on **every** actual request, and confirmed intended success/error/stream paths were called.
- Raw private fake-provider logs still contained the original canary; they were not returned as browser output or published as an artifact.
- A read-only private SQLite query verified at least three real generation history rows retained original canary-bearing `model_name` and `purpose` values.
- Public JSON and SSE checks examined complete wire text plus recursively decoded values/property names, rather than checking only credential-named fields.

## Cleanup and preceding setup failures

All four managed runs were collected. `bash-435` failed before HTTP because Docker 29 suppresses requested port publication on internal networks. `bash-436` reached eight HTTP responses but the runner assumed flat model storage; native migration uses `generations/<bootstrap.generation>/models.json`. These were runner/setup failures, not sanitizer findings. The successful runner uses private bridge HTTP and the bootstrap-authoritative generation path.

Every run removed its own containers, network, and fresh fixture. Final independent Docker inspection found no `novel-native-canary-*` containers or networks. `ai-novel-writer`, `ai-novel-native-test`, and `ai-novel-native-relay` retained their identities and remained running. Only the new runner and this explicitly authorized report were written.

## Exclusions and reproducibility

Not tested: DOM/UI behavior, ingress authentication/TLS, production volumes/credentials, arbitrary unregistered secret detection, credentials shorter than the sanitizer threshold, encoded credentials, credentials split across events, non-OpenAI protocols, cloud credentials, or concurrent-writer behavior. The provider-error SSE path normalizes to a fixed code; literal SSE redaction is positively demonstrated by canary-bearing chunk/full-text values, not by claiming the discarded HTTP error body appeared in SSE.

After an image owner explicitly confirms an immutable image READY, run:

```sh
NOVEL_CANARY_READY_IMAGE=sha256:<ready-image-id> node web/credential-boundary.integration.mjs
```

The runner refuses mutable image tags, prints a nonsecret JSON result, exits nonzero on failure, and cleans its own resources.
