# Native ephemeral response cache

This package owns disposable JSON response documents only. Notification receipts
and offsets, jobs, real or virtual media IDs, search workflow state, credentials,
calendar subscriptions, and the independent local parser/output caches stay
outside it.

## API and limits

- Start with `DefaultOptions()`, override settings, call `Validate()`, then
  `New(ctx, options, sqlStore)`. SQL is required for database/hybrid. No schema
  changes or new module dependencies are needed.
- `Get` returns an explicit hit/miss and a cloned `Entry`. `Set` accepts an
  absolute expiry in the next seven days. Values must contain exactly one valid
  UTF-8 JSON value; bytes and large JSON numbers are preserved.
- `Do(ctx, region, key, ttl, load)` coalesces same-key origin loads in this
  process. Zero TTL bypasses stored reads/writes but retains coalescing. Negative
  or greater-than-seven-day TTL is rejected. Cache faults fail open; origin
  errors, partial origin bytes, and caller cancellation are preserved.
- `DoValidated(ctx, region, key, ttl, validate, load)` additionally rejects wrong
  typed/semantic cache JSON, best-effort evicts it and performs one origin load.
  It validates successful origin bytes before publication. The validator must
  not mutate or retain bytes. Keys must identify the result schema as well as
  the operation/inputs.
- `Generation` + `SetIfGeneration` are available for other origin coalescers.
  Explicit clears and deletes fence older publishers. Region clears
  conservatively advance the whole namespace generation, but delete only that
  region's values. Requests already joined to an older load can receive its
  snapshot; later requests cannot join it or publish its value after a clear.
- `List` returns metadata only, in lexical physical-key order, with 1..100 rows
  and an opaque `Next` cursor. It is a live page, not a snapshot. `GetItem` and
  `DeleteItem` strictly validate keys against the current namespace. Raw request
  keys are SHA-256 hashed and never appear in stored physical key metadata.
- `Stats` reports live physical records and logical stored key+envelope bytes,
  not allocator/RSS usage. Hybrid stats count its SQL authority once. Corrupt
  records can remain represented in metadata statistics until evicted.
- `Close` is idempotent. In-progress cache operations have finite deadlines;
  queued or joined origin callers wake on close/cancellation, and no origin callback is
  started after service close. Already-running caller-owned origins are not
  forcibly canceled or awaited. They cannot publish after close. There are no
  background writes or per-write goroutines.

Default limits are 1024 records, 1 MiB raw JSON, 32 MiB logical stored bytes,
600 seconds default/refill TTL, 30-second operation timeout, 5-second connection
and TLS handshake timeout, and pool size 4. Bounds are enforced by validation:
1..65536 records; 1 KiB..16 MiB value; 1..256 MiB bytes and at least value+256;
1 second..7 days default TTL; 1..30-second operation and 1..5-second connection
timeouts; pool size 1..16. MySQL MEDIUMTEXT additionally refuses envelopes above
16,777,215 bytes. The cache read/validation concurrency is bounded by pool size
in every mode, and origin load concurrency is four. Retained flight records,
including detached pre-clear flights, never exceed MaxEntries.

Each Do call has a 250 ms shared lookup budget for its reads, generation checks
and corrupt eviction, followed by a fresh 100 ms publication budget after its
origin completes. Both are capped by SocketTimeout and the caller deadline.
Optional cache I/O therefore adds at most 350 ms to a response, independently of
origin duration (origin admission/backpressure is separately caller-cancelable).
Cache deadlines never cancel the origin callback. Slow origins can still publish
under the fresh publication budget. Explicit Get/Set/management calls retain the
configured SocketTimeout ceiling.

## Backends and ownership

Memory uses LRU admission with expired-first cleanup and entry/byte quotas.
Database and Redis refuse new admissions at quota; callers can still return the
origin result uncached. Hybrid is memory + SQL, with synchronous SQL write-through
and L1 refill expiry capped by both SQL remaining lifetime and DefaultTTL.
Hybrid L1 is process-local; this increment does not provide cross-process L1
invalidation, distributed origin coalescing, or multi-instance job coordination.

SQL reuses cache_data, with BOTH an exact namespace key-prefix predicate and
positive provider ownership. Values use `anidan_ephemeral_v1`; one generation
row per namespace uses `anidan_ephemeral_meta_v1`. The prefix is
`anidan:cache:v1:<namespace>:`. Generation metadata is a separate owned row;
its finite expiry renews on writes/generation checks and regenerates the token
when expired. The row lock serializes SQL quota admission and generation checks
across connections/instances. SQLite, MySQL and PostgreSQL parameter binding,
byte-length functions and conflict handling are explicit. JSON envelopes carry
absolute UTC expiry; newly owned expires_at indexes use UTC wall-clock values
in the legacy naive column. These rows are excluded from legacy local-time
cleanup. This avoids DST folds or application timezone changes altering expiry
ordering, and does not reinterpret any legacy rows. MySQL binds these UTC wall
indexes/cutoffs as explicit strings, so a DSN loc such as Asia/Shanghai cannot
shift them; PostgreSQL retains its timestamp binding. SQL payload reads use a
server-side byte-length CASE guard before materializing text. Metadata listing
and stats select only the first 96 characters of the versioned envelope, never
the full payload, and list additionally guards physical-key length. They derive
exact UTC expiry from that bounded control header. MySQL expires_at precision is
detected read-only at startup, including legacy DATETIME(0); cleanup timestamps
are rounded upward to its precision so unrelated writes cannot purge live
fractional-TTL entries. Serving/List/Stats still use exact envelope expiry. A
just-expired physical row may conservatively consume quota for up to one column
precision interval (up to one second). Clear counts physical owned records
removed, once per key, which can include such just-expired retained records.
Runtime schemas are never altered.

Legacy SQL management and expiry tasks MUST exclude both provider markers,
globally, then call this service's management methods. They must not bypass
clear fencing, delete generation metadata, double-count hybrid values, or touch
other installations' namespaces. Upstream/default rows are not adopted.

Redis/Valkey supports a standalone endpoint, not cluster redirection or Sentinel.
Allowed schemes are redis/rediss/valkey/valkeys. TLS verifies the server hostname
and certificate chain and requires TLS 1.2+. There is no insecure, proxy or
redirect fallback. URL query/fragment options are rejected. AUTH/SELECT,
dial, verified TLS, reads and writes obey finite deadlines and caller
cancellation. RESP2 bulk/array/frame sizes are checked before allocation; a
protocol fault discards the connection. No URL, credentials or server error
payloads are included in returned transport errors.

Namespace-only Lua owns finite-TTL values, a bounded sorted-set expiry index and
bounded hash accounting/generation metadata. It atomically checks entry+byte
quotas and generation before SET+TTL. It validates indexed key ownership before
any delete. All loops are capped at 65536 entries; pages contain at most 100.
Metadata TTL outlives the maximum value TTL by one minute. There is no KEYS,
FLUSHDB, FLUSHALL, global CONFIG, pickle compatibility, or access to other
namespaces. Namespace must be 1..40 lowercase letters/digits/underscore/hyphen;
regions have the same grammar with a 48-character cap. Configure a unique
namespace when deployments share a Redis database. External deletion/tampering or server eviction
of private Redis index/quota metadata is unsupported and may cause reported
corruption or inaccessible orphan values until their finite TTL expires. Use an
externally managed server policy that preserves namespace metadata (for example
noeviction); this client does not inspect or change global policy and does not
claim a Redis host-memory limit. Do not manually edit its private metadata.

Redis startup failure uses hybrid (or memory when no SQL store exists) only when
RedisFallback is enabled, which is the compatibility default. Invalid options or
URLs remain configuration errors. Runtime faults never silently switch backend.
`Health.Configured` and `Effective` expose the choice. `Degraded` and `Reason`
are sticky historical observations, with LastSuccessfulOperation and
LastFailedOperation timestamps. They are not a live TCP connectivity assertion.
Management failures return errors rather than reporting successful clearing.
The legacy redisMaxMemory option does not configure Redis server memory policy.

## Verification

`go test -race ./internal/cachebackend` and `go vet ./internal/cachebackend`
exercise local modes, quota races, expiry/refill, copy isolation, large numbers,
clear/origin races, cancellation, typed corruption, bounded fake-Redis protocol,
TLS certificate rejection, AUTH cancellation and startup fallback.

For explicitly authorized disposable real protocol verification, set
`ANIDAN_TEST_VALKEY_BINARY` to an official Valkey executable and run the same
tests. The fixture starts a fresh loopback process on a random port with no
persistence and no real credentials, then stops it and removes its temporary
files. It never contacts a configured deployment. Tests cover namespace
isolation, entry/byte quotas, finite TTL, restart-stable keys, generation fencing,
corruption, oversized values and hostile foreign-key index members.

Live SQL opt-in verification uses `TestSQLRealDialects` with
`ANIDAN_CACHE_TEST_MYSQL_DSN` and `ANIDAN_CACHE_TEST_POSTGRES_DSN` pointing to
explicitly selected empty disposable schemas. It refuses any preexisting table
and never uses the application DSN or removes an existing schema. It covers
both database/hybrid, independent SQL connections, timezones and fractional
expiry, quotas, ownership, paging, conditional-publication fencing and reopen.
A second MySQL-only disposable phase exercises legacy DATETIME(0), and another
compares UTC and Asia/Shanghai driver loc connections to the same namespace.
The operator owns disposing of those server processes/schemas afterward.
