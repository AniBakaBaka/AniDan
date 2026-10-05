# AniDan frontend

Derived from [Misaka](https://github.com/l429609201/misaka_danmu_server) at commit `01751526f6e4154bcc8f517481d02b68cb2684a9`, under AGPL-3.0.

Build with Node.js 24:

```sh
npm ci --ignore-scripts
npm run build
```

The Go backend serves `dist/`. For local development, start the backend on port 7769, then run `npm run dev` for Vite on port 5173. The first-run installation page is embedded in the backend and is available before database initialization.

`VITE_ANIDAN_SOURCE_URL` defaults to `/source-code`; any override must offer the deployed version's corresponding source. See [deployment](../README.md), [licensing](../LICENSING.md), [notices](../THIRD_PARTY_NOTICES.md) and [source distribution](../SOURCE_DISTRIBUTION.md).
