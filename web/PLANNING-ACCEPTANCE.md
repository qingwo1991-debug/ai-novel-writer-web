# Native Web planning and authoring acceptance

## Scope and method

- Real Chromium over CDP, ordinary HTTP, native Web gateway and persistent project worker.
- Separate tiny two-chapter project; chapter target 400 Chinese characters.
- Saved LAN8317 profiles: GPT 6.1 Sol for planning/review/revision; Gemini 3.8 Flash for drafting/finalization.
- All project/content mutations use rendered UI controls. No mocked model, injected Web Crypto, store import or direct domain mutation API.
- Raw private evidence remains in ignored `.native-test-data/browser-planning/`; no author prose, credentials or screenshots are published here.
- Provider failures or unknown outcomes are not automatically retried.

## Verified in the bounded live run

| Check | Observed result |
| --- | --- |
| Ordinary HTTP environment | `isSecureContext=false`; native `randomUUID` and `subtle` unavailable; `getRandomValues` available |
| GPT architecture | Four sections completed; explicit character adoption; six physical provider requests |
| Architecture durability | Four confirmed sections persisted after browser refresh and UI reopen |
| GPT chapter blueprints | Two chapter blueprints completed and persisted |
| Gemini chapter-one draft | Complete 369-character draft; short outline plus draft requests |
| GPT review | Complete structured review persisted; unknown findings remained visibly unknown |
| Author confirmation and GPT revision | Explicit additional author note, immutable checklist confirmation, revision generated |
| Revision merge | Author chose revised paragraphs and completed merge; 389-character merged draft persisted |
| Recheck admission | Authoritative cycle read: `merge-committed`, `recheckCount=0`, `recheckDisposition=not-required` |
| No unwanted recheck billing | No `generation:execute` after merge; both original findings remained unverified |

## Driver correction, not a provider/runtime failure

The first driver incorrectly waited unconditionally for a paid post-merge recheck.
Its bounded 240-second response wait timed out after a successful durable merge.
A separate read-only UI reopen confirmed the authoritative cycle was **not-required**,
not a missing required recheck. No model or HTTP error was observed in that run.
The driver now branches on the merge receipt: required rechecks must commit; a
not-required cycle does not cause a fabricated request or a false completed claim.
A required-recheck live branch was not exercised by this particular project.

## Remaining acceptance

Finalization-only follow-up is running against the existing merged chapter, once,
without replaying planning, drafting, review or revision. Finalized postprocessing,
refresh durability and idle pagehide-to-new-tab ownership handoff are not yet claimed.

## Related checks

- SHA-256/UUID helpers and integrity consumers: 127 tests in five suites passed.
- TypeScript `tsc --noEmit -p tsconfig.json` passed.
- SHA-256 tests include published vectors, million-byte input, block boundaries,
  exact Unicode/lone-surrogate UTF-8, native/portable/Node differential checks.
- Browser driver: `web/browser/planning-acceptance.mjs`.
- Runtime production source was frozen before the deployed acceptance image.
