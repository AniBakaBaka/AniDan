# Six compiled-only provider names: bounded public-source feasibility pass

Inspected 2026-09-30. This pass reads repository metadata and source text; it does
not execute compiled modules or downloaded JavaScript. It separates a readable
protocol lead from a working, accepted native adapter.

The pinned Misaka resource [README](https://github.com/l429609201/Misaka-Scraper-Resources/blob/f460c97606cfc1828fb81150fd6f4437d98080dd/README.md)
only identifies a resource repository. Its [version manifest](https://github.com/l429609201/Misaka-Scraper-Resources/blob/f460c97606cfc1828fb81150fd6f4437d98080dd/resources/linux-x86/versions.json)
contains provider versions/hashes, not domains, request schemas or corresponding
source contracts. Repository tree inspection found no readable provider source.

An independent public project, `huangxd-/danmu_api`, supplies readable candidates
at verified commit `fc1b7ff6add61d8af24c9bf978253273833f5afc`. Its root license is
[GNU AGPL v3](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/LICENSE),
preserved in `LICENSES/danmu-api-AGPL-3.0.txt`. Exact inspected file digests are in
`docs/evidence/public-provider-protocols.json`. This establishes provenance for
inspection/adaptation, not a complete transitive licensing audit.

| Provider name | Verified public evidence | Concrete prerequisite / next boundary |
|---|---|---|
| migu | [migu.js](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/danmu_api/sources/migu.js) gives search, content/episode data, duration and bounded-time comment URLs. [migu-util.js](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/danmu_api/utils/migu-util.js) gives the response codec | Sufficient readable structure for a native implementation attempt. Verify the public site's session/SID and response codec assumptions without adopting a borrowed identity, then fixture and live acceptance. Implementation work has been authorized separately |
| xigua | [xigua.js](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/danmu_api/sources/xigua.js) describes mobile search HTML, `episodes_list`, page duration and public comment-list query fields | Sufficient readable structure for a native implementation attempt. Current page/schema access and nonempty comment retrieval still need verification. No cookie/key is hard-coded in this inspected path. Implementation work has been authorized separately |
| mddcloud | [maiduidui.js](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/danmu_api/sources/maiduidui.js) explicitly targets `mob.mddcloud.com.cn` and implements search/detail/comment payloads | Readable Android signing/payload code exists, with empty appToken, a shared signing constant and fixed deviceNum. Signing alone is not an authorization blocker. The public website returned HTTP403 in this environment; an independent current source for the signing/identity bootstrap remains unverified. No account requirement is established, and no borrowed constant/identity was transmitted |
| hongguo | [hongguo.js](https://github.com/huangxd-/danmu_api/blob/fc1b7ff6add61d8af24c9bf978253273833f5afc/danmu_api/sources/hongguo.js) has readable web parsing plus Gorgon/Argus/Ladon-style request signing and API hosts | Public web search/detail parsing is a feasible independent path; the homepage is reachable. The Android comment path has readable signing code and empty cookie/token defaults, so account login is not proven necessary. What remains unverified is generating a valid installation/device identity and current comment API acceptance. No borrowed identity or signed request was used |
| girigirilove | [Published Kazumi rule](https://github.com/Predidit/KazumiRules/blob/main/giriGiriLove.json) identifies `ani.girigirilove.com`, a search path and chapter HTML selectors | Search/episode parsing has a public lead, but the rule also enables CAPTCHA handling and supplies no comment-pool mapping/API. Need accessible permitted pages plus a verified comment endpoint/identifier contract; no CAPTCHA bypass is part of this implementation |
| ezdmw | The resource manifest identifies version 1.0.4 and a binary hash. No source/domain contract was established by the bounded repository/search pass | Need the provider's authoritative domain/configuration and a readable request/response contract or corresponding source. Do not infer a domain from its module name or invent endpoints |

The candidate code's error handling and concurrency policies are not adopted
automatically. Native adapters must retain AniDan's cancellation, response/total
bounds, small concurrency limits, explicit partial-download errors and safe URL
resolution. Public text availability does not prove service access, quota,
authentication entitlement or equivalence to the current Misaka binary.

## Implementation result after the feasibility pass

Migu and Xigua now have native adapters and passing protocol/race fixtures. Migu
also verifies its fresh request-SID behavior against an official public search
bundle (URL/hash in `docs/evidence/migu-official-search.json`); no fixed SID was
borrowed. Its live search/discovery succeeded for one title, but its first pool
was empty and a second search had an incompatible response. Xigua's bounded live
requests returned HTTP500. These outcomes are recorded in `benchmarks/live/` and
do not count as nonempty comment acceptance. The four remaining providers still
have the prerequisites above; their code/keys/identities were not deployed or
transmitted.

### Public-access distinction (follow-up)

The Mddcloud public homepage returned403 on the single bounded inspection request;
that constrains this environment's verification, not whether a native adapter can
exist. Hongguo's public homepage returned HTML without an account. Request signing
in these readable adapters is not by itself a bypass or a hard implementation
blocker. Fixed identifiers must not be mistaken for a user credential, nor reused
as one without establishing their role. Current protocol acceptance and legitimate
identity initialization remain to be established; no claim that all four names
are inherently impossible is made.

Hongguo public search and detail have since been implemented without Android
signing. A Go live pass returned10 search results and66 public video IDs in9.478
seconds. The page reported3 accessible episodes, so the ID inventory does not
promise playback/comment access. `benchmarks/live/hongguo.json` is the recorded
partial acceptance; no signed comment request or account credential was used.

### Girigirilove explicit challenge (post-stage1)

A bounded request to the published rule's search path returned the site's
`ds-verify` CAPTCHA input, image and search verification button, with no matching
search-result containers. The path was stopped. No challenge submission,
identity substitution, alternate host or protected search retry was performed.
The response hash and observed selectors are recorded in
`docs/evidence/girigirilove-search-challenge.json`. This distinguishes a current
access prerequisite from the independently unresolved comment-pool mapping.

An independent historical gap was closed in the same development pass: Le now
implements native public search and complete episodic-series pagination and
passed a real 9,002-comment source-to-player chain. This does not change the
remaining prerequisites for Mddcloud, Hongguo comments or Ezdmw. Already migrated
XML remains servable even when fresh provider retrieval is unavailable.
