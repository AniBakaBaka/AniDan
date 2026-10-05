# 第三方来源与致谢

感谢 [l429609201](https://github.com/l429609201) 及 [Misaka Danmu Server](https://github.com/l429609201/misaka_danmu_server) 的所有贡献者提供弹幕服务、接口、前端及数据结构方面的工作；感谢 [Misaka-Scraper-Resources](https://github.com/l429609201/Misaka-Scraper-Resources) 的维护者及弹幕源相关贡献者。

AniDan 是独立维护的兼容重写项目，不代表上游官方项目，也不暗示上游作者为本项目提供担保或额外许可。

## 上游来源

- 主项目参考提交：`01751526f6e4154bcc8f517481d02b68cb2684a9`，根许可证 GNU AGPL v3；原文保留在 `LICENSE` 与 `LICENSES/AGPL-3.0.txt`
- 历史 scraper 参考提交：`300ad904b3bed5a490c18855ca2903bdff5e1792`，当时根许可证 GNU AGPL v3；逐文件来源注释应继续保留
- 资源仓库参考提交：`f460c97606cfc1828fb81150fd6f4437d98080dd`，根许可证 GNU GPL v3；参考其元数据不表示打包了其中的二进制模块
- 若交付中包含上游前端、资源或文档，其许可证和原有通知继续适用

原有声明的差异、网络源码义务和进一步核对事项见 [许可证说明](LICENSING.md)；实际对应版本的源码提供方式见 [SOURCE_DISTRIBUTION.md](SOURCE_DISTRIBUTION.md)。弹幕内容和服务商标不因代码许可证而获得额外授权。

上游 README 列出的参考项目亦在此保留，供追溯具体功能的来源（不表示这些项目赞助或认可 AniDan）：

- [弹弹play](https://www.dandanplay.com) 与其开放 API
- [danmaku](https://github.com/lyz05/danmaku)
- [emby-toolkit](https://github.com/hbq0405/emby-toolkit)
- [swagger-ui](https://github.com/swagger-api/swagger-ui)
- [Bangumi-syncer](https://github.com/SanaeMio/Bangumi-syncer)
- [imdbsource](https://github.com/wumode/MoviePilot-Plugins/tree/main/plugins.v2/imdbsource)
- [MoviePilot](https://github.com/jxxghp/MoviePilot)
- [bangumi-data](https://github.com/bangumi-data/bangumi-data)

## 构建依赖

实际 Go 依赖以 `go.mod` / `go.sum` 为准，前端依赖以实际交付的包清单及锁文件为准。发布前应生成固定版本清单，收集实际复用/分发依赖的版权和许可证全文，并保留前端字体、图标及静态资源的单独通知。当前此说明不替代完整的软件物料清单，也不宣称所有依赖已经审核完成。

## MCP 兼容性参考

MCP 的上游工具名称/参数摊平行为参考 `fastapi-mcp` 0.4.0（Tadata Inc.，MIT），其许可证保留于 `LICENSES/fastapi-mcp-MIT.txt`。生产 Go 程序不运行或依赖该 Python 包。固定上游的控制 API 声明经 AST 提取生成契约，不执行原业务函数；生成工具依赖 FastAPI/Pydantic，其版本记录在契约中。

## Additional readable provider protocols

Migu, Xigua and Hongguo public-web protocol compatibility refers to [huangxd-/danmu_api](https://github.com/huangxd-/danmu_api) at commit `fc1b7ff6add61d8af24c9bf978253273833f5afc`, specifically `danmu_api/sources/migu.js`, `xigua.js`, the public-web portions of `hongguo.js`, and `utils/migu-util.js`. Its GNU AGPL v3 text is preserved in `LICENSES/danmu-api-AGPL-3.0.txt`; source digests and feasibility boundaries are in [public-provider-protocols.json](LICENSES/provenance/evidence/public-provider-protocols.json) and [PROVIDER_FEASIBILITY.md](LICENSES/provenance/PROVIDER_FEASIBILITY.md). Other pinned provider sources are retained in [PROVIDERS.md](LICENSES/provenance/PROVIDERS.md). No downloaded JavaScript or compiled provider binary is executed by the Go server. Availability of this source does not establish upstream binary parity or legal clearance for unrelated embedded code.

## Go runtime and embedded timezone data

Go runtime/standard-library code is distributed under the Go BSD-style license,
retained in `LICENSES/Go-BSD-3-Clause.txt`. The standalone CLI embeds the Go
`time/tzdata` database so configured timezones do not depend on host installation.
The toolchain's IANA time-zone provenance notice is retained in
`LICENSES/IANA-time-zone-NOTICE.txt`; IANA identifies its timezone database as
public-domain data. See https://www.iana.org/time-zones.

## Exact frontend JSON identifiers

The frontend uses `lossless-json` 4.3.0 by Jos de Jong (MIT), pinned to the
official npm registry artifact in `web/package-lock.json`. Its verbatim license
is retained in `LICENSES/dependencies/npm/lossless-json_4.3.0/LICENSE.md`.
Transport scope, security handling and artifact integrity are documented in
[FRONTEND_ID_TRANSPORT.md](LICENSES/provenance/FRONTEND_ID_TRANSPORT.md).

These provenance documents retain their historical source-attribution scope. Historical evidence links may refer to records removed during repository cleanup; they do not establish current validation. The deployed version must include its own corresponding source.
