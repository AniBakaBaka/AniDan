# Provider and danmaku implementation status

## Scope and evidence

This is a pure-Go implementation, not a loader for the Python provider package.
The current source package at
[`f460c97606cfc1828fb81150fd6f4437d98080dd`](https://github.com/l429609201/Misaka-Scraper-Resources/tree/f460c97606cfc1828fb81150fd6f4437d98080dd)
contains 17 compiled CPython 3.12 providers. No `.so` or `.pyd` provider module was executed to develop these packages.
Historical provider Python was inspected without executing it; isolated parser
comparison benchmarks do not introduce a Python runtime dependency. A compiled module appearing in the
catalog does **not** imply that its capabilities have been implemented.

Ten readable historical provider implementations were inspected at
[`300ad904b3bed5a490c18855ca2903bdff5e1792`](https://github.com/l429609201/misaka_danmu_server/tree/300ad904b3bed5a490c18855ca2903bdff5e1792/src/scrapers).
The root license at that commit is AGPL v3, not a permissive license. The Go
protocol implementations are credited to l429609201 and upstream contributors
and carry AGPL-3.0-only notices. Historic comments credit additional C#/JS
implementations; this does not establish a complete transitive provenance audit.

Dandanplay authentication follows its
[official API documentation](https://doc.dandanplay.com/open/):
Base64(SHA-256(AppID + decimal Unix seconds + API path + AppSecret)). Only the
signature, app ID, and timestamp are transmitted; the secret is never sent as a
header or query parameter.

## Capability matrix

“Yes” means executable Go implementation with synthetic local HTTP fixture tests.
It does **not** mean current production access or complete parity with the pinned
2026 compiled module. Live acceptance requires geographically appropriate access,
valid credentials where required, and representative real titles.

| Provider | Search | Episodes | Comments | Known scope / exact remaining blocker |
|---|---|---|---|---|
| bilibili | Yes | Yes | Yes | WBI search, PGC, UGC multipart and primary-pool segmented protobuf. Authorized cookies may be needed. Subtitle/other extra pools, auth actions and every source-specific filter are not ported |
| dandanplay | Yes | Yes | Yes, after config | Registered AppID and AppSecret required. Official search/bangumi/comments; no alternate-secret retry or proxy fallback |
| gamer | Yes | Yes | Yes | Historical HTML layout and form POST; no challenge solving, cookie refresh, region bypass, or provider-specific search-language conversion |
| hanjutv | Yes | Yes | Yes | Aggregate search, series detail and monotonic cursor comments. Search metadata uses supplied summary; no extra per-title detail enrichment |
| mgtv | Yes | Yes | Yes | Search, month tabs and `opbarrage` cursor endpoint. CDN/bullet-ws fallback and variety/movie ordering heuristics not ported |
| tencent | Yes | Yes | Yes | Historical search RPC, explicit episode-tab RPC and complete indexed comment segments. Generic episode-pagination fallback, movie and variety ordering heuristics not ported |
| iqiyi | Yes | Yes | Yes | Mobile search, decode/baseinfo/album pagination and duration-based zlib XML. Stage16 streams XML after conservatively budgeted compressed-fetch windows (default2, ceiling4). Signed v3, Brotli/protobuf and variety-month fallbacks not ported |
| sohu | Yes | Yes | Yes, duration windows | Stage19 keyless GBK list and exact source duration; all official 300-second player windows, fixed page 1, live 9,575-comment import/player sample. Server-side history/sampling and other categories remain unverified; explicit 60-second caller windows retained. [Scope](SOHU.md) |
| le | Yes | Yes | Yes | Public HTML search and complete 50-item episode API pagination; live chain verified with 9,002 comments. Episodic series only; variety discovery unverified. Comments require parseable page duration |
| renren | No | No | Plaintext static only | Signed/encrypted API and app endpoints not ported; encrypted static payloads are rejected |
| youku | Yes | Yes, after config | No | Registered `youkuClientId` required for OpenAPI pagination. Native pagination has fixture tests; no registered credential was available for live acceptance. Comment signing/token flow remains unverified |
| ezdmw | No | No | No | Authoritative domain and readable API/comment contract not established in the bounded public-source pass |
| girigirilove | No | No | No | Published HTML rules exist; a bounded search returned an explicit CAPTCHA form. No challenge bypass or alternate-identity retry; comment-pool mapping/native adapter remain unverified |
| hongguo | Yes | Yes | No | Public web search/detail JSON, live verified10 results/66 IDs. Comments identity/bootstrap and signed-API acceptance unverified; no signed request is sent |
| mddcloud | No | No | No | Readable signing/payload reference exists; native adapter and legitimate identity setup unverified. Public website returned403. Signing alone is not a prohibition |
| migu | Yes | Yes | Yes | Public search/content API and duration-bounded encrypted comment payloads, fresh request SID, 4-worker segments. Golden codec and protocol fixtures pass; see live evidence for populated-pool acceptance |
| xigua | Yes | Yes | Yes | Public mobile HTML discovery and unsigned five-minute comment API, bounded 4-worker segments. Fixtures pass; two search probes and one known-video probe returned HTTP500 |

There are 11 providers with implementations of all three interface methods,
3 partially implemented providers and 3 explicit unavailable providers. These
counts describe method coverage, not full product/provider parity.

Additional common gaps: provider-specific calendars, subscription discovery,
configurable provider actions, login QR flows, caching policies, proxy-domain
replacement, dynamically installed plugins and title/season inference parity.
Search only retrieves the first result page. Older historical adapters default
`Season` to 1; newly added URL metadata/Migu/Xigua adapters preserve unknown season
as zero. Structured season inference is not complete. Non-numeric
source comment IDs cannot be represented in the `int64` CID contract; some historical adapters map them to zero, while the new Sohu path rejects invalid IDs. Player output regenerates CIDs from zero, as upstream does.

## Configuration and concurrency

- `DefaultRegistry()` registers the complete 17-name catalog; unavailable methods
  return typed `UnsupportedError`, not a successful empty result
- `Registry.Search(ctx, keyword, names...)` runs at most the configured number of
  workers (1–32), preserves source ordering, and returns partial results **and**
  joined errors. Callers must expose those errors rather than label partial
  results complete
- Search requests have a default 30-second source timeout. HTTP requests default
  to 20 seconds, paginated comment downloads to a 3-minute overall deadline
- Each HTTP provider limits concurrent requests to 4, each response to 32 MiB,
  comments to 500,000, a 64 MiB aggregate decoded-comment budget (text + p + 64 bytes
  per record estimate), and pagination/segment count to 300. Overrides are bounded
  by the configuration schema
- All HTTP work uses request contexts. HTTP errors, JSON/schema errors, nonmoving
  cursors, missing segment counts and resource limits remain visible
- On a failed required segment the download fails; previously collected comments
  are not returned as a complete success. iQiyi's documented historical empty
  segment 404 is handled as an empty segment, with later segments still fetched
- Provider endpoints are fixed in code. User-facing configuration cannot supply
  arbitrary API roots. `BaseURLs` exists for controlled tests/custom construction
- Redirects reject HTTPS downgrades and cross-host destinations outside a fixed
  provider-host allowlist; headers are cleared on any permitted host change
- URL resolution accepts exact known hosts and known path shapes, no credentials,
  explicit ports, arbitrary schemes, localhost, lookalike hosts or URL fetching

Use `Registry.ConfigSchema(name)`, `ValidateConfiguration(name, settings)`,
`Configure(name, settings)`, and `Configuration(name)`. Validation and changes
operate on detached copies; published provider instances remain immutable for
in-flight requests. Runtime credentials/config must be loaded from the actual
installation database by the server. Merely migrating stored settings does not
activate a provider without that wiring.

Accepted common keys: `userAgent`, `timeoutSeconds`, `maxSegments`, `maxComments`,
`maxResponseBytes`, `maxDownloadBytes`, `segmentWorkers` and `proxyURL`, plus
`{provider}Cookie` except Dandanplay. Legacy
`{provider}UserAgent` and `cookie` aliases are recognized. Dandanplay accepts
`dandanplay_app_id` / `dandanplay_app_secret` and their camelCase aliases.
`Configuration` masks cookies and secrets as `********`; passing that mask keeps
the existing value, while an empty string clears it. Unknown fields explicitly
fail. `proxyURL` supports HTTP, HTTPS, SOCKS5 and SOCKS5h with isolated cloned
transports, masked credentials and redacted network errors. An explicit empty
proxy forces direct routing. Server-level use_proxy selection must supply the
chosen proxy or explicit empty value. Alternate secrets, arbitrary provider-domain overrides, FlareSolverr and unsupported binary-provider settings explicitly fail. Stage17 adds the documented [accelerate gateway route](ACCELERATE_PROXY.md) only after exact operator trust; imported configuration alone cannot authorize forwarding. Gateway access includes forwarded credentials and responses, and all existing request bounds remain.

## URL and collection helpers

`ResolveURL` is read-only parsing. `Registry.ResolveEpisode` and provider-specific
optional `ResolveEpisode` methods require a single unambiguous episode when a URL
only resolves to a media ID. Bilibili `epNN` and `?p=N` target the exact episode or
multipart page. Direct platform episode IDs are returned from known URL syntax;
this does not itself establish that the episode exists or is accessible.

`Bilibili.CollectionMetadata` verifies optional UGC membership; collection
imports and `Episodes("collection:<seasonId>:<mid>")` share bounded complete
listing with exact identity checks and primary-CID resolution. The ordinary
video URL remains a separate scope. Details, deliberate ordering/multipart
policy and the current live412/CAPTCHA boundary are in
[Bilibili collection compatibility](BILIBILI_COLLECTIONS.md). Bilibili short-link
expansion remains unimplemented.

## XML and transformations

`internal/danmaku` provides streaming bounded XML parsing/writing, internal and
player p-field conversion, offsets, half-open splits, stable time-sorted merging,
full-p-plus-text deduplication, RE2 blacklist filtering, mode filtering/mapping,
color modes, likes styling/stripping, sampling and Chinese phrase conversion.

- Default parse limits: 64 MiB input, 1,000,000 comments, 64 KiB per comment text,
  and 32 XML levels. Invalid control characters are removed. Malformed comment
  fields are skipped by default or rejected with `StrictNodes`; malformed XML,
  invalid UTF-8, directives/entities and exceeded limits are explicit errors
- Bilibili eight-field and Dandanplay three/four-field p attributes normalize to
  time,mode,font,color,source. Explicit `FromPlayer` resolves ambiguous low RGB
  values in four-field Dandanplay attributes
- `Player` removes only the font field, preserves the optional source suffix and
  assigns zero-based CIDs. It returns internal `Comment` values; HTTP handlers
  must serialize the intended player `{cid,p,m}` wire shape without `t`
- Transformations return new slices and never mutate persisted input. Negative
  offset results are discarded; split boundary comments enter the second part
  rebased to time zero. Merge deduplication includes the full normalized p value
- Sampling and randomized color choices are intentionally deterministic. Sampling
  is evenly spaced in chronological order, **not** a claim of exact parity with
  upstream's stochastic three-minute-bin quota algorithm
- Repeated text is not automatically collapsed to `Xn`. Exact duplicate removal
  is available; retaining repeated distinct comments avoids changing source data
- Likes supports heart colors/outline, brackets, text and number styles, minimum
  five likes and configurable hot threshold. Stored legacy white-heart/fire
  suffixes can be restyled; existing suffixes can be stripped
- XML writing is a normalization/export operation, not a byte-preserving legacy
  migration. Migrations must copy raw files to preserve every original attribute

`Convert(text, mode)` supports 0 unchanged, 1 traditional→simplified and
2 simplified→traditional. It embeds **all** four character/phrase dictionary
files from OpenCC data in longbridgeapp/opencc v0.3.13, pinned to commit
`eec5c563bc3271ddc2d3a4438d798f560d4187a2`. Files, checksums, Apache-2.0 license
and attribution are under `internal/danmaku/dictionary`. Initialization is lazy
and maps immutable. A separately written longest-phrase matcher avoids linking
OpenCC's Go runtime dependency chain. It does not claim every regional conversion
or exact equivalence to native OpenCC's full segmentation pipeline. The discarded
runtime candidate depended on cedar-go with unresolved GPLv2 compatibility;
that runtime is not imported by this package.

## Validation

Run:

```sh
go test -race ./internal/danmaku ./internal/provider
go test -cover ./internal/danmaku ./internal/provider
go test -run '^$' -bench BenchmarkParse10000 -benchmem ./internal/danmaku
go test -fuzz FuzzParse -fuzztime 10s ./internal/danmaku
go test -fuzz FuzzProtobuf -fuzztime 10s ./internal/provider
```

Local fixtures cover every implemented method, official Dandan signing,
Bilibili WBI/protobuf and bigint IDs, XML variants/escaping/error limits,
nonadvancing pagination, empty middle segments, contextual cancellation, bounded
worker/HTTP concurrency, HTTP 429, forbidden redirects, malformed responses,
configuration atomicity/redaction, exact-episode resolution, incomplete
collections and every embedded dictionary key (with phrase precedence).
Fixtures are synthetic protocol examples. They are not captured/current service
responses and must not be represented as live acceptance results.

### Live acceptance observations (2026-09-30 UTC)

Public, credential-free search → episodes → first-episode comments succeeded for
four adapters. Each downloaded pool was then saved through the Go server's
SQL/XML persistence and served by a real loopback HTTP server under both player
API prefixes. Every returned comment's CID, parameter string and text was
compared, not just its HTTP status or count.

| Source | Search results | Episodes | Retrieved comments | Comment download/decode | Whole source chain |
|---|---:|---:|---:|---:|---:|
| Tencent | 15 | 178 | 25,259 | 18.725 s | 28.910 s |
| Bilibili | 2 | 17 | 1,008 | 1.046 s | 10.577 s |
| Iqiyi | 10 | 206 | 4,880 | 9.836 s | 31.977 s |
| Le (post-stage1) | 3 | 76 | 9,002 | 32.822 s | 50.087 s |

These are single observations from this development environment, not comparative
throughput or completeness guarantees for every title. Adapter tests did not
attach the server's additional local quota policy. Complete source IDs, dates,
HTTP request counts, errors, local HTTP timings, response digests and reproduction
steps are in [`benchmarks/live`](../benchmarks/live/README.md).

Live testing found and corrected two adapter bugs: Tencent segment names contain
safe hierarchical paths (`t/v1/0/30000`); Iqiyi's decode API uses the historical
primary `pcw-api.iq.com`, while the mainland hostname returned 404. Synthetic
regression fixtures now cover those actual protocol shapes.

Other observations in the same bounded pass:

- Youku returned 11 search results; episode pagination was subsequently implemented
  behind a registered `youkuClientId`, with no credential available for live
  acceptance. Comments remain explicitly unavailable
- MGTV and Gamer search returned HTTP 403; no challenge bypass was attempted
- Hanjutv rejected search parameters with `rescode=-2`; Sohu returned the
  explicit empty-result response `data.is_empty=1` for the selected query
- Dandanplay failed locally before any network request because registered AppID
  and AppSecret were not configured
- Renren does not implement search/episode discovery; compiled-only ezdmw,
  girigirilove and mddcloud remain unimplemented

A failed or unavailable chain is not represented as an accepted source.
`TestLivePipeline` requires explicit opt-in and a nonempty comment pool;
`TestLiveRetrievedCommentsPlayerHTTP` accepts a captured XML path and performs
no additional external requests.

`Registry.ResolveMedia(ctx, rawURL) (SearchResult, error)` retrieves genuine
Bilibili PGC/UGC media title, ID and cover from the source API. PGC IDs become
canonical `ssNN`; UGC IDs become BV/av. Unknown structured season remains zero,
and UGC media type is `other` rather than guessed from its title. Exact episode
selection remains the separate `ResolveEpisode` operation. The Stage14 development increment also adds Tencent/iQiyi/Le source-declared URL metadata, verified media/episode identities and bounded fields; see [URL import behavior](URL_IMPORT.md). Remaining providers return typed unsupported errors for URL metadata. An episode title is never substituted for a missing series title.


## Download performance work

Bilibili protobuf and Tencent indexed-JSON segments use bounded parallelism
(default 4, configurable 1–4). Segment order is retained before stable time sort;
failures cancel siblings and no partial result is mislabeled complete. HTTP
connections are reused. Concurrent same-episode requests share one in-flight
fetch; the last subscriber leaving cancels it. Results are copied per caller,
so one caller cannot mutate another's result. Completed results are not cached
inside adapters; the server owns parsed/output cache and persisted-file refresh
semantics. Dandanplay and all historical adapters use the same coalescing wrapper.

Decoded Bilibili comments use direct canonical construction, avoiding a
format/parse round trip. Tencent segments use a typed JSON decoder, including
JSON-string or object styles. Already canonical provider comments are deduplicated
without another normalization pass. Aggregate count and byte-estimate budgets
bound multi-segment/paginated memory, in addition to each response's byte limit.

Reproducible synthetic download benchmark source, raw logs, checksums and results
are under `benchmarks/fetch/`. Measurements compare this Go pipeline's sequential
and four-worker modes; they are not real Internet throughput and not a comparison
against the unknown current compiled provider binaries. Optional local rate
limiter cooldowns/quotas, server caches, database writes and player serialization
are separate integration layers and excluded from these adapter benchmarks.

Registry hooks `SetSearchObserver` and `SetRequestLimiter` provide bounded
per-provider search metrics and external admission/HTTP-status feedback without
query, result-text or credential disclosure. HTTP 429 remains an explicit error;
Retry-After feedback lets the local limiter impose future cooldowns. No remote
quota or signed-provider entitlement check is bypassed.

## Youku and Hanjutv follow-up investigation

The native Youku episode adapter now supports bounded OpenAPI pagination with an
explicitly supplied, registered `youkuClientId`. It keeps the existing configured
transport/proxy, cancellation, request limits and error redaction. It checks every
page against the advertised total, rejects duplicates/partial pages and caps
results at 10,000 episodes and the configured page budget. The key is masked and
supports masked-value round trips. No historical Huawei partner key/package is
bundled. Fixture verification covers 103 items across two pages and rejection
paths; no valid registered client ID was available for live episode acceptance.

[Youku's official API reference](https://cloud.youku.com/docs?id=46) lists
`client_id` as required even for public video metadata. A real unauthenticated
request to the historical episode endpoint returned code 1004, `Client id null`.
This is a configuration/entitlement requirement, not a successful empty list.

The historical MTop bootstrap endpoint returned anonymous cookie names
`_m_h5_tk` and `_m_h5_tk_enc`, with `FAIL_SYS_TOKEN_EMPTY` in the same response.
Cookie values were not saved in evidence. That demonstrates bootstrap behavior,
not a verified comment request. Inspecting a public player page reached an
explicit CAPTCHA challenge. No challenge was executed or bypassed, and no signed
comment call was attempted. Comments remain an explicit unavailable capability.
See `benchmarks/live/youku-*.json` for bounded observations.

Hanjutv's historical search parameters (`keyword`, `scope=101`, `page=1`) were
rejected with `rescode=-2` / `bad request params`; the public `hanju.com` page
returned 403. No current parameter/signature contract was established from these
sources. The blocker is recorded in `hanjutv-parameter-blocker.json`; unknown
parameters are not guessed or repeatedly brute-forced.

## Migu / Xigua readable-source expansion

Migu and Xigua are implemented from independently inspected public AGPL source,
not their current compiled Misaka modules. Exact pinned provenance and remaining
prerequisites for the four unavailable names are in [PROVIDER_FEASIBILITY.md](PROVIDER_FEASIBILITY.md).
Migu's official public search bundle confirms a locally generated 64-character
request SID; AniDan generates a fresh nonce instead of reusing the reference's
fixed SID. Official script URL/hash and observations are in
`docs/evidence/migu-official-search.json`. The response codec is verified with
independently produced OpenSSL golden ciphertext and strict framing, padding,
UTF-8 and count limits. An empty successful pool does not pass live acceptance.
Xigua's three bounded live probes returned HTTP500, including one known video;
no alternative-host or CAPTCHA bypass followed. Its fixtures verify response
shape, duration-derived segments, bigint IDs, limits and incomplete-download
failure behavior. Every external error remains visible.

Migu live observations on 2026-09-30: `斗罗大陆` returned one title and40 episodes;
the first episode's bounded segment fetch completed with an empty pool (12 HTTP
requests,25.542 seconds). `狂飙` returned a response lacking the expected search
list (one request,7.038 seconds). Neither passes nonempty-comment acceptance;
further live probes stopped. Reports are `migu-empty-title.json` and `migu.json`.

Hongguo public discovery was implemented and checked end-to-end through the Go
adapter: search10 → selected series66 video IDs,9.478 seconds/2 public HTTP
requests. The page exposes the full ID inventory but reports only3 episodes as
currently accessible; no playback/comment entitlement is inferred. When the page
supplies only IDs, display labels use their source-list ordinal (`第1集`, etc.) and
are not represented as official episode titles. No episode permalink is guessed.
Comments fail explicitly before network access until a legitimate anonymous/API
identity path is established. Report: `benchmarks/live/hongguo.json`.

## Le public discovery expansion (post-stage1)

Le search now parses the public `So-detail` metadata without evaluating JavaScript.
The parser accepts a bounded flat string object and rejects executable expressions,
duplicate keys, nesting, malformed escapes and trailing input. Source metadata
preserves an unknown season as zero. Unrelated subject/clip result types are skipped;
missing layouts are explicit errors rather than successful empty results.

The current official detail bundle and page tabs establish the public
`d-api-m.le.com/detail/episode` endpoint. AniDan requests 50 items per page and
verifies the advertised total, returned page, page size, per-item media ID,
contiguous ordinals and unique video IDs. Partial pages, changing totals and page
limits fail the entire discovery. No shared mutable search cache is needed.
This episodic-series path does not claim variety discovery or full compiled parity.

A real Go chain on 2026-09-30 returned 3 search titles, 76 episodes and 9,002
comments for series `73868`, episode `1578861`, in 50.087 seconds / 14 requests.
The captured pool passed SQL/XML persistence and exact CID/p/text comparison
for every comment under both player prefixes. Both responses contain 658,474
bytes with SHA-256
`b4411a8c2cbb109e32191e6130199ef9de9eacb83126ff7b44490d06120b150b`.
See `benchmarks/live/le.json`, `le-server.json`, `captured-pools-le.json` and
`docs/evidence/le-public-protocol.json`. Captured XML remains outside the source
tree and is excluded from release artifacts. These are incremental development
results; the delivered stage1 archive and its receipts were not altered.

The separate Girigirilove search probe returned an explicit verification form.
No challenge was solved, alternate identity/host used or protected search retried.
`docs/evidence/girigirilove-search-challenge.json` records the response digest and
stopping point. Retrieval gaps do not prevent serving already migrated XML.

## Stage16 iQiyi fetching verification

See [the bounded input/canonical budget contract and controlled/online evidence](../benchmarks/iqiyiparallel/README.md). Default adaptive concurrency is two, not an unconditional four; one streaming decoder enforces per-record limits before deduplication. The combined tracked payload allowance is128MiB at defaults and is not a process RSS ceiling. Controlled fixtures showed reduced elapsed/allocation volume with small RSS increases. One real known episode returned4,880 identical comments with four requests in both modes; the individual timings do not establish a causal Internet speedup. Other unimplemented iQiyi formats and provider gaps above remain unchanged.

## Stage19 Sohu public-window acceptance

On 2026-10-02 the official keyless list returned all 67 declared episode entries with GBK encoding and source duration. A guarded known-episode import fetched 11 official windows in 13 HTTP requests and persisted 9,575 comments; both player prefixes matched exact P/text and sequential CIDs, with no warm refetch. Elapsed 14.923s is one uncontrolled observation. This is duration-window coverage, not proof of every historical server comment; [raw evidence and limits](../benchmarks/sohu/stage19/README.md). No inherited app identifier was transmitted. Earlier Sohu search-empty observations remain historical evidence for their specific query.
