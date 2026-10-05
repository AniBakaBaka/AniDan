# Exact frontend identifier transport

## Boundary and contract

JavaScript's native JSON decoder can turn the numeric identifier
`9007199254740993` into its neighbor `9007199254740992`. The shared UI transport
must decode original response text before Axios's default `JSON.parse` runs.

`web/src/utils/idTransport.js` exports:

- `asIdentifier(value)`: preserve a nonempty, whitespace/control-free string;
  convert only a safe integer Number to its decimal string. Unsafe Numbers,
  fractional Numbers, nonfinite values, BigInt and invalid types throw.
- `sameIdentifier(left, right)`: compare valid normalized IDs. Missing, empty,
  invalid or unsafe inputs return false, suitable for rendering and lookups.
- `parseAPIJSON(text, { identifier = false })`: parse using a lossless JSON
  grammar implementation, then convert numeric ID fields into strings. `id`,
  `ids`, camel-case `…Id`/`…Ids`/`…ID`/`…IDs`, snake-case `…_id`/`…_ids`, and the
  audited aliases `cid`, `aid`, `bvid`, `vid`, `pid`, `mid`, `gid`, `uid`, `egid`
  are identifiers. Nested objects are inspected; scalar/nested arrays under ID
  keys inherit ID context. Null and empty optional fields remain unchanged.
- `readAPIJSON(response, options)`: read original native Fetch response text and
  apply that same decoder.
- `stringifyAPIJSON(value, options)`: validate the final JSON tree, including
  `toJSON` output, and return `{ body, exactIdentifiers }`. Numeric ID inputs must
  already be safe; exact decimal-string inputs stay strings.
- `normalizeAPIParams(params)`: validate object query parameters before Axios
  stringifies them. URLSearchParams are already textual and retain their normal
  encoding behavior.

Ordinary metrics, counts, pagination, indices, times and decimal values retain
normal JavaScript Number semantics, including the normal Number limitations.
No global JSON or BigInt behavior is modified. String IDs are never passed
through Number or parseInt. Numeric JSON ID tokens must use decimal integer
notation; fractional or exponent ID tokens fail closed. Opaque string IDs and
zero-prefixed string IDs are preserved exactly.

Root scalar/array payloads lack a field name. Endpoints that return IDs at the
root must opt in with `identifier: true`, or Axios `responseIdentifier: true` /
`requestIdentifier: true`. Root counts and lists of numeric indices are therefore
not accidentally converted. No blanket inference from an arbitrary unsafe root
number is made.

## Mutation compatibility

Axios uses the shared decoder in place of its default response transform. The
request transform validates JSON before Axios's normal encoder. The shared
request interceptor adds `X-AniDan-Exact-IDs: decimal-string-v1` to **every**
POST/PUT/PATCH/DELETE request, including bodyless URL-targeted writes,
metrics-only JSON, form and binary requests. This identifies the validated
frontend protocol even when the target ID exists only in a path. A stale bundle
without this marker can be refused by the server for unsafe-range UI mutations.
The marker does not change form/binary encoding. The Go boundary
uses this explicit header to accept quoted integers only where the destination
type is an integer; string fields and ordinary numeric metrics keep their type.
Legacy clients without the header keep the prior JSON contract.

`web/src/apis/index.js` validates and URI-encodes every identifier interpolated
into a resource URL. Danmaku offset/split/merge and source-split operations no
longer call JSON.stringify before shared validation. An already-rounded Number
is rejected before the adapter can send a mutation. Preencoded JSON can preserve
large numeric ID tokens because the codec reads their original text, but no
codec can reconstruct a Number rounded before it received that input.

FormData, URLSearchParams, Blob, ArrayBuffer/view and streaming request bodies
bypass JSON serialization. The JSON default does not convert FormData to an
object or override URLSearchParams' form encoding. Explicit binary/text response
types bypass JSON decoding. Cancellation retains Axios's cancellation identity
and AbortSignal propagates through wrappers. No mutation is retried here.

## Ingress audit ledger

| Boundary | Treatment |
| --- | --- |
| `apis/fetch.js` | Shared Axios request/response transforms and query validation |
| `apis/index.js` | ID path guards; removed early serialization of ID mutations |
| `apis/container.js` | Native status/confirmation/error JSON and progress SSE use codec; hash IDs and progress validation unchanged |
| `apis/responseCache.js` | Native structured JSON uses codec; detail intentionally retains original raw JSON text |
| `VersionModal.jsx`, `Scrapers.jsx`, `useRateLimitSSE.js` | Component/hook JSON event ingress migrated by the view audit |
| `CalendarView.jsx` | Native Fetch JSON migrated by the view audit |
| `library/index.jsx` | Imported episode-group JSON migrated by the view audit |
| `LocalItemList.jsx` | Record cache uses lossless decoder and a new exact-ID version key; old rounded cache is not reused |
| `MatchFallbackSetting.jsx` | Embedded selected-token ID arrays use explicit root-ID mode |
| WebAuthn embedded options | Kept as protocol JSON; these contain base64url string credentials and numeric timeout/algorithm fields, not database integer IDs |
| Raw logs, presentation-only JSON formatting, generic UI storage | Not treated as mutation ID ingress; retained existing behavior |

New API ID field aliases or new root-ID endpoints must be added deliberately,
with tests; arbitrary numeric metrics are not coerced globally.

### Endpoint root and non-JSON audit

The current native handlers and `apis/index.js` callers were cross-checked.
**No current wrapper requires `responseIdentifier` or `requestIdentifier`.**
Those options remain explicit, tested support for future scalar-ID or root-ID-
array endpoints. They must not be enabled across all endpoints.

| Endpoint/caller | Actual root shape and decision |
| --- | --- |
| GET `/api/ui/library/episodes-by-title` / `getInLibraryEpisodes` | `libEpisodeIndices` selects DISTINCT `episode_index`, returning `[]int64` episode numbers. `SearchResult.jsx` uses these as existing episode indices. Keep Numbers |
| GET `/api/ui/tokens`, `/api/ui/ua-rules`, `/api/ui/anime/groups`, library/source/episode lists | Arrays of records, or paginated objects; IDs are named fields and already normalized |
| GET `/api/ui/media-servers/{id}/libraries` | `mediaLibraries` returns `[]media.Library` with a named string `id`, not primitive IDs |
| GET media/local show-season endpoints | `mediaSeasonList` returns records containing season, counts, year and poster, not ID scalars |
| GET scheduled tasks, provider/source lists, metadata search and TMDB episode-group actions | Arrays/objects of records with named IDs; no root scalar-ID mode |
| PUT `/api/ui/scrapers`, `/api/ui/metadata-sources` | `scrapersUpdate` / `metadataSourcesUpdate` read arrays of configuration records. Their numeric `displayOrder`/timeouts must remain numbers |
| Bulk delete/refresh/import/reorder/merge/calendar subscription | Handlers read objects with named `…Ids`, `items`, `operations`, `shows` or `seasons`; nested named IDs already normalize |
| Creation/import/job responses | Records or objects with `id`, `animeId`, `sourceId`, `taskId`, etc.; no bare scalar ID |
| Match-fallback/poster-proxy token settings | `{value: "JSON-array text"}` wrappers. The view deliberately parses embedded arrays with root-ID mode; setters preserve the exact encoded string |
| Logs and available webhook services | Root string arrays contain display text/service names and stay unchanged |

The only form encoder in `apis/index.js` serves login, MFA verification and
passkey login verification. It now validates any named ID before constructing
URLSearchParams, while leaving opaque credential JSON strings and auth values
unchanged. The already-built URLSearchParams/FormData transport bypass is
intentional: values are strings by then and the original Number type cannot be
recovered. New callers must validate numeric identifiers before appending them.
Current multipart callers upload files, not numeric database-ID fields.

Native mutation audit: container confirm/restart/update identify a container with
validated SHA-256-like hash strings and one-use confirmation headers, not numeric
database record IDs. Response-cache deletes address a namespaced opaque key or
region. Neither wrapper has a large numeric record-ID mutation requiring the
marker. Other direct native Fetch sites in components are read-only version,
cached-status and calendar-title requests. No native record-ID write was found;
new ones must use the exact-ID marker and validation before URL construction.

Object query parameters such as `server_id`, `task_id`, `media_id` and `searchId`
are validated before Axios encodes them. Manual URLSearchParams builders carry
pagination, time ranges, filters, formatting switches and backup filenames;
their ID paths use `identifierPath`. Tests exercise real wrapper calls and
Axios `getUri` to verify exact decimal query values and rejection before send.

## Parser provenance and security

The exact dependency is `lossless-json` **4.3.0**, MIT, from the
[official npm registry](https://registry.npmjs.org/lossless-json/-/lossless-json-4.3.0.tgz).
Its upstream [parser documentation and source](https://github.com/josdejong/lossless-json)
were inspected along with the installed package. The lockfile pins the tarball
and integrity:

`sha512-ToxOC+SsduRmdSuoLZLYAr5zy1Qu7l5XhmPWM3zefCZ5IcrzW/h108qbJUKfOlDlhvhjUK84+8PSVX0kxnit0g==`

Install scripts were disabled. The verbatim MIT notice is retained at
`LICENSES/dependencies/npm/lossless-json_4.3.0/LICENSE.md` and recorded in
`docs/DEPENDENCIES.json`.

The dependency assigns ordinary object keys. Before it runs, a native JSON
grammar-validation pass rejects `__proto__`, `constructor` and `prototype`
keys, including escaped equivalents. Every numeric result from this preliminary
pass is discarded. Only the lossless pass supplies actual API values. Private
number-token objects prevent user JSON from impersonating a numeric wrapper.
Conflicting duplicate keys, malformed input and unsafe keys fail closed.

## Verification

`node --test web/tests/id-transport.test.mjs` exercises the installed Axios
1.11.0 transforms, including response → selection → mutation roundtrips for
adjacent IDs above 2^53 and int64 maximum. The focused suite covers ordinary
metrics, safe IDs, URL-only/bodyless mutation marking, nested arrays, root-ID options, escaped JSON, raw strings,
malformed input, prototype pollution, toJSON validation, cancellation,
FormData/URLSearchParams/binary bypass, URL guards and native JSON/SSE ingress.

The focused transport suite contains 18 tests, including endpoint-root,
configuration-record, query-string and auth-form regressions. The complete
frontend unit suite passed 55 tests after the companion view fixes.
Focused lint on the transport/API files passed. Full repository `npm run check`
remains blocked by existing JSX lint debt (193 findings in the observed run).
These are synthetic transport tests, not live-provider or browser acceptance.

## Go request, durable-job and stale-page boundary

The `decimal-string-v1` marker enables range-checked string-to-integer conversion
only at integer-typed fields in the target Go schema. No floating-point conversion
is used; int64/uint64 overflow, fractional/exponent strings, whitespace and
malformed values fail. String fields, quantities represented as JSON numbers,
interface values and custom JSON decoder semantics remain intact. Opaque JSON
values keep their meaning, but the marked request normalization pass does not
promise byte-for-byte whitespace/key-order preservation. This is not applied to
signed external webhook request bytes. Durable typed task parameters and spooled
comment IDs use the same exact integer boundary so a queued operation does not
lose the accepted ID later. Old numeric task parameters remain accepted.

Ambiguous case-insensitive DTO field names and multiple case aliases for the
same declared field fail closed. Use the exact schema name. Unmarked control and
player API contracts are retained. Marked conversion costs an additional JSON
pass on UI writes and typed durable parameters; no performance speedup is claimed.

Old UI bundles can already contain rounded IDs. For `/api/ui/` mutations, the
server rejects unmarked high IDs in **named** route placeholders, named query
parameters and JSON ID fields/arrays before the corresponding operation runs.
The legacy anime-group dispatcher has an explicit named-ID guard. It does not
scan arbitrary text, credentials, numeric substrings, ordinary metrics or player/
control requests. Path/query rejections return 428; existing JSON handlers keep
their validation error status (typically 400/422). Current Axios mutations all
carry the marker, including bodyless DELETE and URL-only writes.

The marker is a client-format declaration, not authentication or proof that an
ID was never rounded earlier. It cannot reconstruct an already-corrupted ID.
**After upgrading both server and frontend, close all old tabs/PWA windows and
reload the updated application before editing data.** Do not transplant old page
state. The old local item cache is bypassed by a versioned exact-ID key; original
cache data is not silently deleted. Use a clean frontend build to avoid old bundles
in the PWA precache. Previously delivered Stage1–4 archives remain immutable and
retain their old UI limitation.

### Actual installed-client HTTP regression

`TestExactJSONInstalledAxiosToGoMutations` starts the full Go handler and imports
the installed Axios client, actual shared interceptors and real `apis/index.js`
wrappers in Node. Two neighboring rows `9007199254740992` and
`9007199254740993` are listed and selected independently. The second is refreshed
through a durable job and synthetic provider, edited, and deleted by both bulk
JSON and bodyless path-only API routes. The first remains unchanged. Counts and
episode indices remain Numbers. The exact marker is observed on actual requests.
No browser, real database records, accounts or public provider traffic is involved.
The fixture is explicitly skipped where Node/npm dependencies are absent; release
verification installs them and requires these cases to execute and pass.

Additional Go fixtures verify rejected stale numeric/string body and path IDs,
untouched neighboring rows, no provider calls on rejected requests, preserved
control API int64 paths, authentication enforcement, typed maps/arrays/pointers,
custom decoders, int64/uint64 limits, durable payloads and unchanged body-size
bounds. Evidence and final frozen-source checks are in
`docs/evidence/increment-5/` and the separate archive receipt.
