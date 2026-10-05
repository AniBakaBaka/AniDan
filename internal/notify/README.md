# Notification implementation

Implemented Go transports: Telegram Bot API, Server酱³ Bot API at
`bot-go.apijia.cn` (not the unrelated legacy SendKey endpoint), and enterprise
WeChat/WeCom token, text/image-message and native-menu APIs. Pinned Misaka source informed the protocol
and UI contracts; its AGPLv3 license/attribution applies. No real notification was
sent during development; all send tests use local fixture servers.

Server integration includes the eight notification UI operations, the two
callback methods, schemas, physical-table channel CRUD, masked credential
round-tripping, connection/test-message delivery, public-domain verification,
queued outbound events and queued authenticated bot commands.

The complete historical server conversation, receive lifecycle, QR login,
fallback collage and safety contracts are in `docs/NOTIFICATIONS.md` inside the
separately preserved documentation archive identified in the
[project README](../../README.md). Current scope and acceptance limits remain in
the [summary](../../docs/性能对比总结.md); this package's operational boundaries
are retained below.

## Supported behavior

- Text sends check both HTTP status and service result; failure/invalid recipients
  are not labeled successful. WeCom caches tokens and retries once only for the
  explicit expired-token code 42001
- `SendProgress` and `EditProgress` support one Telegram text task message with
  generated bold headings/labels and a code-styled task ID.
  They use the existing bounded transport and safe routing, with link previews
  disabled and no image,
  button, delete, replacement-message or retry fallback. WeCom and SC3 return
  `ErrUnsupported`. The configured `chat_id` is required; a message recipient
  cannot override it. Exact nonzero signed integer recipients and Telegram
  usernames are supported; floating-point configured IDs are rejected
- Ordinary Telegram text notices bold a nonempty title. Explicit generated body
  spans support only disjoint `bold`, `code` and `text_link` ranges; literal
  source/user text is never interpreted as Markdown or HTML. Body rune offsets
  are converted to UTF-16 units for the Bot API. Local limits remain 4096 runes
  and 16 KiB of visible UTF-8 text, with at most 32 body spans, 16 KiB encoded
  entities and 64 KiB encoded text payload; these are local resource policies,
  not claims that Telegram defines its character limit in UTF-16 units
- After final aggregation/clipping and text/image-mode selection, an ordinary
  titled Telegram text part may add a visible login-required task-list link.
  Only `custom_api_domain` can supply a public HTTPS origin, using the fixed
  `/task` path and optional port 443. Credentials, query, fragment, other paths,
  private literal addresses and malformed hosts are rejected. Missing/unsafe
  settings or a size limit omit this optional addition. Hostname validation is
  syntactic: it does not prove DNS answers, ownership or reachability. No relay,
  provider URL, token or task receipt is reused as a navigation target
- Rich text disables link previews. Other providers retain plain text;
  photo/public-image captions, private QR replies and image-only parts retain
  their existing behavior. Explicit spans on a photo are rejected before any
  part is sent. Formatting metadata is runtime-only and is not persisted in
  event payloads. Progress rendering does not read settings or perform network
  work while holding its scheduler mutex
- Progress deduplication hashes canonical visible text, sorted entities and a
  rendering version. A valid active receipt with an older plain hash may receive
  one normally throttled edit. Pending, uncertain and terminal states retain
  their existing fences; upgrading formatting never authorizes a replacement
  send. Queue pause does not pause the separate progress worker: disable the
  channel/subscription to stop subsequent operations; an in-flight request may
  finish
- A progress send succeeds only with a complete provider `Message` response.
  `message_id` must be a positive canonical integer and `chat.id` a nonzero
  canonical signed integer, both parsed with `UseNumber` and stored as strings.
  Numeric recipients must match the returned chat exactly; usernames must match
  `chat.username` case-insensitively. Later edits use the returned numeric chat
  ID, preventing a username from redirecting the edit to another chat
- A private persisted `ProgressReceipt` retains `MessageID`, `ChatID`,
  `Recipient` and `Binding` together. Its HMAC binds the identifiers to the
  channel ID/type, current bot token and configured recipient. Modified receipts
  or changed identities are rejected before HTTP. The server owns durable
  admission, lifecycle ownership and cancellation fences; a receipt alone is
  not an exactly-once guarantee. Never expose these fields in public status
- Failed/ambiguous progress requests yield errors without fabricating a receipt;
  transport errors and malformed successful responses wrap
  `ErrDeliveryUncertain` and retain context/limit classification. No automatic
  resend occurs. The shared transport intentionally does not parse non-2xx
  provider bodies or `Retry-After`: Telegram HTTP 400 “message is not modified”
  therefore remains a redacted error, as do 429 responses. A successful
  non-inline edit must return the same message and chat identifiers; a boolean
  `true` result is rejected. Protocol references checked 2026-10-02:
  [sendMessage](https://core.telegram.org/bots/api#sendmessage),
  [editMessageText](https://core.telegram.org/bots/api#editmessagetext),
  [Message](https://core.telegram.org/bots/api#message), and
  [Chat](https://core.telegram.org/bots/api#chat). This implementation retains
  the pinned Misaka source baseline and limits task progress to an editing-capable
  channel; it does not claim all-provider progress parity
- Fixed official service origins or explicitly configured safe proxy/HTTPS relay routes (see [outbound routing](#outbound-routing)), redirects forbidden, no ambient proxy, 15-second send timeout,
  1 MiB response limit, four concurrent sends/registration requests. Polling has a
  separate four-request pool and timeout five seconds longer than its configured
  1–30 second wait (default 10). URLs/tokens/proxy credentials are
  absent from public network errors
- Credentials are masked in API responses; `********` preserves existing values
  during edits. Events remain opt-in using the exact event key or explicit `*`
- `emitNotification(ctx,notify.Event{Type,Title,Text})` submits a durable bounded
  `notification_delivery` job. `NotifyEvent` is the direct dispatcher. Delivery
  jobs do not recursively notify about themselves
- Telegram and SC3 callbacks require the server webhook API key. Configured
  Telegram/SC3 secret-token headers are additionally verified in constant time
- Telegram and SC3 `Service.Poll` use their real `getUpdates` endpoints. Responses
  are limited to 100 updates, sorted by numeric ID, and checked for malformed IDs
  before returning any cursor progress. Unsupported and unauthorized updates are
  skipped while advancing the returned cursor; transport/JSON/ID failures return
  the original offset. The server must admit each update durably before storing
  its checkpoint, and only then store the final skipped-event offset. Polling
  channels reject webhook POSTs. SC3 supports flat `message.chat_id`, nested
  `message.chat.id`, and `message.from.id` fallback, as in the pinned adapter
- `Message.Image` uploads actual PNG/JPEG bytes, without any URL fetch. Telegram
  sends multipart `sendPhoto` with caption and unchanged opaque callback keys;
  WeCom uploads temporary media then sends an image (and a separate text message
  if present). Limits: 5 MiB Telegram, 2 MiB WeCom, 4096 per dimension, 4 Mi pixels,
  two concurrent image decodes, 1024-rune Telegram captions and 2048-byte WeCom
  text. Invalid/unsupported images and partial image+text failures are errors;
  no silent text fallback occurs. SC3/WeCom inline buttons are explicitly rejected:
  the server owns their numbered-choice fallback and single-use action mapping
- Native menus are changed only by explicit `RegisterCommands`: Telegram
  `setMyCommands` (up to 100 entries) and WeCom `menu/create` (1–15 entries, grouped
  in three sets of five; child labels at most 60 UTF-8 bytes). Telegram
  `RegisterWebhook` and `DeleteWebhook` preserve pending updates. SC3 registration
  is unsupported. `Send`, `Poll`, `Test` and construction never register or delete
  webhooks/menus automatically. Switch a previously registered Telegram bot to
  polling by explicitly removing its webhook first. The server exposes these
  actions through its authenticated operator registration route
- WeCom callbacks require signed AES-256-CBC encryption, strict PKCS#7 padding,
  matching corporation identity, valid XML and a timestamp within five minutes.
  Plaintext/unsigned legacy fallback is intentionally rejected
- Interaction allowlists are fail-closed: an empty admin/allowed list permits no
  incoming commands. Allowed users can read status/library/tasks and run search;
  only configured admins can import, refresh or cancel tasks
- Supported commands: `/help`, `/status`, `/library [keyword]`, `/tasks`,
  `/task <id>`, `/search <provider> <keyword>`, admin `/refresh <episodeId>`,
  `/cancel <taskId>` and `/import <provider> <mediaId> <title>`
- Admin search/URL results now offer an explicit edited-import conversation:
  select exact episodes, page the list, select all/none, change movie/TV type and
  season, review, then confirm. Whole-result import remains separate. Selection
  retains the source order, opaque IDs and episode indices, including zero;
  it never renumbers a subset. Initial season follows the existing whole-result
  default of at least one; explicitly editing it to zero is supported
- Edited drafts expire five minutes after opening. The preview is bounded to
  1,000 source rows including excluded rows and 256 KiB of conservatively counted
  snapshot data. Each active draft reserves 528 KiB for its snapshot, validation
  copy and state within a 4 MiB runtime budget, allowing at most seven drafts.
  Oversized/ambiguous/invalid previews refuse instead of silently truncating
  “all.” Drafts live only in existing channel/sender/chat sessions; active retired
  sessions stay charged until their lease ends. No durable draft registry exists
- Confirmation rechecks persisted enabled/admin/channel settings, routing and
  provider identity, and a fresh exact filtered source listing. Only selected
  rows enter the ordinary `generic_import` with `Edited=true`; configured storage
  title/type/season recognition and metadata enrichment still apply. Changes,
  expired callbacks, wrong owners and empty selections cannot submit. The short
  final draft claim contains no database or queue operation; an admission already
  started after confirmation may finish despite a later cancel/configuration
  change. Caller cancellation bounds admission; an admitted task has its normal
  manager lifetime. Unknown queue results clear the draft and require checking
  `/tasks`, with no automatic retry. Source state is not frozen after admission
- Duplicate callback IDs have persistent seven-day receipts. The receipt and
  pending task are admitted in one SQL transaction through `SubmitWithReceipt`;
  failures leave neither behind and duplicates return the admitted task ID.
  Atomic admission does not claim exactly-once external message delivery. The
  cache-management UI protects active receipts and polling cursors
- Public-domain tests reject private/reserved/link-local/loopback addresses,
  credentials and nonstandard ports; validate every DNS answer; pin resolved
  addresses for dialing; use no environment proxy; and forbid redirects. A random
  probe filename is removed afterward

## Outbound routing

- Telegram's explicit `telegram_api_proxy` takes precedence and appends
  `/out/api.telegram.org/bot<TOKEN>/<METHOD>`. Otherwise `useProxy=true` selects
  the configured global HTTP/SOCKS proxy; `useProxy=false` uses the official
  origin. The supported global modes are `http_socks`, or legacy `none` with
  `proxyEnabled=true`. Invalid, missing or accelerate-mode selections fail
  visibly without direct fallback
- SC3's `sc3_api_proxy` appends `/out/bot-go.apijia.cn/bot<TOKEN>/<METHOD>` and
  receives the runtime webhook key as `X-Relay-Key`. WeCom's `wecom_proxy`
  preserves its prefix through the first `/cgi-bin`, expands a trailing `/out`
  to `/qyapi.weixin.qq.com/cgi-bin`, or appends `/cgi-bin`. WeCom receives the
  relay key only with `wecom_proxy_relay_auth=true`. SC3/WeCom global
  `useProxy=true` is unsupported
- Reverse relays receive provider credentials and message/image content, so
  configuring one is an explicit operator trust decision. Only public HTTPS
  port 443 relays are supported: no userinfo, query, fragment, ambiguous paths,
  private/reserved addresses, redirects or ambient/forward proxy. Every DNS
  answer is validated before dialing a pinned address with verified TLS
- Telegram forward proxies support `http`, `https`, `socks5` and `socks5h`,
  including explicit local/LAN proxy endpoints and hop-specific authentication.
  Provider origins remain fixed HTTPS with certificate/hostname verification;
  disabling TLS verification is unsupported. Environment proxy variables are
  not used
- Text/image calls, tests, polling and registration use the selected route.
  Operations retain their initial settings snapshot; edits do not replay or
  retarget an in-flight request. Stored proxy fields are masked and
  `********` preserves an existing value. No relay, real credential or VPS
  tunnel is provisioned by the fixtures

Historical routing verification records were removed during repository cleanup. The supported routing behavior is described above.

## Explicit remaining gaps

Rich photo/caption editing, deletion, arbitrary Markdown/HTML rendering,
quote-block/poster-link template parity, SC3 image uploads/native menu or webhook
registration, WeCom polling/webhook registration and VPS tunnels are not
implemented. Generated text entities implement a bounded presentation subset,
not byte-compatible upstream Markdown templates. Global forward proxy is Telegram-only; private/HTTP reverse
relays are intentionally rejected. See [outbound routing](#outbound-routing)
for supported relay paths and trust boundaries. Server on-demand
poster/collage commands use the supported bounded byte-upload paths. Configuring
active unsupported modes returns an explicit error.
Existing stored JSON is preserved, not silently reinterpreted. Outbound event
coverage depends on callers invoking the event hook; implementing a dispatcher
alone does not establish all upstream lifecycle events or aggregation parity.

Bot commands use the ordinary task queue and visible task states. A failed send
is visible as a failed notification job. There is no automatic retry on ambiguous
network errors, because the service may already have accepted the message.

`BaseURLs`, `Client` and `Now` are fixture/configuration injection points; configure
them before concurrent use. Tests use local HTTP servers only, including polling,
multipart bytes, menu/webhook registration, expired tokens, cancellation,
malformed/oversized replies, callback authorization and outbound-slot isolation.
