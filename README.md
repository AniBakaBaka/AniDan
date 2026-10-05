# AniDan

Go 弹幕服务，基于并致谢 [Misaka Danmu Server](https://github.com/l429609201/misaka_danmu_server/) 及其贡献者。当前数据库兼容结构对应上游 v2.8.9（`01751526f6e4154bcc8f517481d02b68cb2684a9`）。运行时不需要 Python，不加载上游二进制 `.so/.pyd` 弹幕源。

## Docker Compose 启动

在本仓库目录执行：

```sh
docker compose up -d --build
docker compose logs anidan
```

打开 `http://服务器地址:7769`，填写日志中的一次性安装码，然后选择数据库、填写管理员账号和密码。默认 SQLite 无需额外数据库；MySQL / MariaDB、PostgreSQL 可在页面填写主机、端口、数据库名、用户名、密码及 TLS 设置，需预先创建空数据库并授予访问权限。

无需预填 `.env`、管理员密码或 JWT 密钥，也无需选择 profile。新安装自动生成随机 JWT 密钥。安装配置保存在 `anidan-data` 卷的 `/data/anidan.json`，管理员密码散列存入数据库，不写入配置文件。重启后自动读取配置并进入登录页面，安装入口关闭。`docker compose down` 保留数据；`docker compose down -v` 会删除数据卷。

默认端口为 7769，可通过 `ANIDAN_PORT` 修改；`ANIDAN_BIND_IP` 控制宿主机监听地址。仓库 Compose 使用 UID/GID `0:0`，丢弃全部 capability 后仅添加 `DAC_OVERRIDE`，以读写属主不同的原版挂载目录；容器根文件系统仍为只读。镜像单独运行时默认用户仍为 `10001:10001`。数据库位于宿主机时填写 `host.docker.internal`，数据库需监听容器可访问的接口并允许相应账号连接；数据库位于其他容器时，将服务加入共同网络并填写数据库服务名。

## 直接使用 Misaka 数据

此模式直接连接现有 MySQL / PostgreSQL 数据库，不导出、不复制数据表、不改写表结构。账号、密码散列、API 令牌、弹幕库、元数据、设置及历史记录继续存储在原库；XML、NFO、图片等文件仍使用挂载目录。原版程序仍需停止，两个服务不能同时运行写入同一套数据。

原有 `app.log.4`、`app.log.5` 等范围外日志归档会保留，不参与 AniDan 的三份备份轮转，也不计入新日志的 40 MiB 上限。旧日志格式、大小或权限导致文件日志无法启用时，服务仍会启动，日志继续输出到容器日志，可通过 `docker compose logs anidan` 查看。

1. 备份原数据库和完整 `config` 目录，停止原版应用，保留数据库运行。
2. 在 `compose.yaml` 的 `volumes` 中添加原版完整目录的挂载（替换宿主机路径）：

   ```yaml
   - /absolute/path/to/misaka/config:/app/config
   ```

   使用仓库 Compose 时可直接读写原目录，无需修改原文件属主。目录必须挂载为读写；自定义存储目录也需按原路径挂载，并通过配置的 `readRoots` / `writeRoots` 授予应用相应访问权限。
   若数据库连接或 JWT 设置写在原版 Compose 的 `environment` 中，再增加一个文件挂载，无需手动抄写参数：

   ```yaml
   - /absolute/path/to/misaka/docker-compose.yml:/app/config/docker-compose.yml:ro
   ```

3. 启动 AniDan，在安装页面选择“直接接入 Misaka 旧库”，点击“保存并启动”。默认自动读取 `/app/config/config.yml` 和挂载的原 Compose，合并数据库连接、JWT 密钥、算法及时区，并检查表结构、账号、引用文件与写权限。完成后使用原账号登录。

无需导出 JSON、上传文件、手动转换路径或重新填写已有账号密钥。安装配置保存到独立 `/data` 卷，原配置文件不会被改写。此模式继续使用原库，不复制为新库。

若使用旧 Compose 提示“数据目录不可写”，应用当前 Compose 的 `user: "0:0"` 和 `cap_add: [DAC_OVERRIDE]`，重新创建容器即可，原文件属主保持不变。此配置不能绕过只读挂载、SELinux 或远程文件系统服务端的访问控制。

若自行选择以非 root 用户运行（镜像默认为 `10001:10001`），可在宿主机 root 终端用 ACL 授权，仅作用于原 `config` 目录并保留文件属主（替换为实际路径；需要系统安装 `acl` 工具且文件系统支持 ACL）：

```sh
setfacl -R -m u:10001:rwX /absolute/path/to/misaka/config
find /absolute/path/to/misaka/config -type d -exec setfacl -m d:u:10001:rwx '{}' +
```

第一条授权现有文件与目录，第二条让以后创建的文件继承授权。若自行设置了容器运行用户，请将 `10001` 换成实际 UID。授权后可以直接重试保存；非 root 应用不能自行提升权限或修改宿主机授权。只读文件系统、磁盘空间或 inode 用尽也会导致写入失败，需按实际错误处理。

只有部署差异需要打开“高级选项”：

- **数据库地址改变**：填写“仅修改数据库主机”，保留自动读取的端口与账号密码。数据库容器需与 AniDan 加入同一网络；原来的 `127.0.0.1` 不会自动指向另一容器或宿主机。
- **配置不在默认位置**：修改挂载目录，或指定原配置 / Compose 文件路径。自动识别目录内的 `docker-compose.yml`、`docker-compose.yaml`、`compose.yml`、`compose.yaml`，多份文件不会猜测选取。
- **使用了额外环境覆盖**：当前支持上游 `app` 服务的明确 Compose 环境值；`${变量}`、`env_file`、自定义服务结构与多文件覆盖不能推断实际值。指定实际 `config.yml` 可跳过自动 Compose，再补充原版实际 JWT 等覆盖值；需要替换全部数据库参数时勾选“手动填写完整数据库连接”。

MySQL / PostgreSQL 的 `db-data` 是数据库引擎文件，不能仅挂载给 AniDan 直接读取。保留原数据库服务运行，AniDan 自动使用读取到的连接信息访问它。缺失密钥、数据库不可达或文件权限不足时，安装页会提示处理；不会生成新密钥冒充原账号密钥。

接入保留所有原表数据，但不代表所有版本或原版功能完全等价。不同版本的未知表会保留；已知表缺少字段或类型不兼容会拒绝接入，不能承诺任意最新版本旧库都可使用。动态 Python 弹幕源不能在 Go 服务中执行。当前没有使用真实用户旧库做整体验收。

搜索黑名单、全局/单剧分集过滤及弹幕源分集黑名单支持旧规则中的前瞻、后顾等语法（如 `(?!...)`）。普通规则使用 Go 原生正则，需要扩展语法时使用纯 Go 的 regexp2 兼容引擎；不删除或跳过原规则。规则最多 16 KiB，单段输入最多 64 KiB，兼容匹配设置短超时（引擎定时检查，非精确截止时间）；超时或语法错误会明确返回，不能保证所有 Python 正则完全等价。此兼容范围不包含其他功能中独立使用的正则。

## 构建与配置

构建使用 Go 1.27、Node.js 24、Python 3。Python 仅用于打包对应源码。

```sh
make build
./bin/anidan -setup
```

原有环境变量和显式配置方式仍可使用：

```sh
ANIDAN_ADMIN_PASSWORD='your-initial-password' ./bin/anidan
./bin/anidan -config /path/to/config.json
```

`-setup` 启用首次安装；已保存配置优先自动加载，显式 `-config` 路径优先于自动查找。`ANIDAN_*` 环境变量优先于配置文件。Docker 只设置 `ANIDAN_CONFIG_DIR=/data`，因此页面选定的数据库及旧数据目录不会被默认环境变量覆盖。

| 环境变量 | 用途 |
|---|---|
| `ANIDAN_CONFIG_DIR` | 安装配置保存目录，默认 `data`；Docker 为 `/data` |
| `ANIDAN_LISTEN` / `ANIDAN_DATA_DIR` | 监听地址 / 应用数据目录 |
| `ANIDAN_DRIVER` / `ANIDAN_DSN` | SQLite、MySQL、PostgreSQL 连接 |
| `ANIDAN_ADMIN_USERNAME` / `ANIDAN_ADMIN_PASSWORD` | 无安装向导时的首次管理员初始化 |
| `ANIDAN_JWT_SECRET` / `ANIDAN_JWT_ALGORITHM` | 会话与原版 TOTP 密钥兼容 |
| `ANIDAN_TIMEZONE` | 默认 `Asia/Shanghai` |
| `ANIDAN_PUBLIC_URL` | HTTPS 公网 origin，用于 Passkey |
| `ANIDAN_EXTERNAL_API_KEY` / `ANIDAN_WEBHOOK_API_KEY` | 外部 API / webhook 密钥 |
| `ANIDAN_READ_ROOTS` / `ANIDAN_WRITE_ROOTS` | 额外读取 / 写入根目录，按操作系统路径列表分隔 |

已有网页迁移入口“设置 → 参数配置 → 数据库 → 迁移旧弹幕库”仍可将旧库和文件复制到独立 SQLite 目标，再单独启用。它与直接接入模式不同：复制模式不继续使用原数据库写入。

## 开源来源

- [许可说明](LICENSING.md)、[第三方声明](THIRD_PARTY_NOTICES.md)
- [对应源码供应](SOURCE_DISTRIBUTION.md)、[前端构建](web/README.md)
- [通知实现](internal/notify/README.md)、[缓存配置](internal/cachebackend/README.md)

部署构建保留 `/source-code` 对应源码下载。感谢 Misaka Danmu Server、Misaka Scraper Resources、huangxd-/danmu_api 及各依赖贡献者。
