# 许可证与来源说明

AniDan 是对 Misaka Danmu Server 行为、接口及数据结构进行兼容实现的 Go 项目。受上游许可约束的翻译、修改及复用部分继续保留原有义务；更换编程语言不构成另行授权。本项目不宣称已获得上游作者认可、额外授权或商用许可澄清。

## 已核对的基线

| 项目 | 固定提交 | 根许可证 |
| --- | --- | --- |
| [misaka_danmu_server](https://github.com/l429609201/misaka_danmu_server) | `01751526f6e4154bcc8f517481d02b68cb2684a9` | GNU AGPL v3 |
| [Misaka-Scraper-Resources](https://github.com/l429609201/Misaka-Scraper-Resources) | `f460c97606cfc1828fb81150fd6f4437d98080dd` | GNU GPL v3 |
| 历史可读 scraper 基线 | `300ad904b3bed5a490c18855ca2903bdff5e1792` | 当时根 LICENSE 为 GNU AGPL v3 |

根目录 `LICENSE` 原样保留主项目的 GNU AGPL v3 许可证文本。新增代码按 GNU AGPL v3 提供；各第三方依赖保留各自许可证。不能据此把第三方文件改标 MIT、Apache 或其他宽松许可，也没有将第三方素材、数据和商标一并授权。

## 上游声明存在不一致

固定基线的主项目根 LICENSE 是 AGPL v3，但 Dockerfile 第 131 行的 OCI 标签写作 MIT；README 的用户责任一节还含有个人学习、研究或非商业用途的表述，推广须知另有平台限制。这里记录差异，不把 Docker 标签当作宽松重许可依据，也不对这些声明的法律效力作结论。涉及商业分发、重新许可或对外部署前，应向权利人澄清并核对适用义务。

来源：[LICENSE](https://github.com/l429609201/misaka_danmu_server/blob/01751526f6e4154bcc8f517481d02b68cb2684a9/LICENSE)、[Dockerfile](https://github.com/l429609201/misaka_danmu_server/blob/01751526f6e4154bcc8f517481d02b68cb2684a9/Dockerfile#L131)、[README](https://github.com/l429609201/misaka_danmu_server/blob/01751526f6e4154bcc8f517481d02b68cb2684a9/README.md#L62-L88)。

## 分发和网络服务

请阅读完整 `LICENSE`，尤其第 5、6、13 节。分发适用的修改版本或运行适用的网络服务时，需按许可证提供相应源码、许可证及所要求的通知。源码包应包含实际运行版本的源码及所需构建文件；仅有致谢文字不能替代这些义务。官方说明：[GNU AGPL](https://www.gnu.org/licenses/agpl-3.0.html)。

## 编译插件和继承来源

当前资源仓库的 `.so` / `.pyd` 是 Python 编译模块，不是 Go 插件。本项目不因资源仓库标注 GPL 就声称掌握其对应源码，也不以二进制包替代源实现审计。若未来加入这些模块，必须单独审核其源代码提供及分发要求。

历史 scraper 注释提及 `jellyfin-plugin-danmu`、`danmu_api`、`parserYouku.js` 等来源；复用具体实现时还需核对相应版本的来源和通知。不得删去已有版权或来源声明。来源不明的片段不应标为“完全原创”或“已完成许可审计”。

最终依赖清单与通知见根目录 `THIRD_PARTY_NOTICES.md`。此文件记录工程来源及待澄清事项，不是法律意见。
