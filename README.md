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

默认端口为 7769，可通过 `ANIDAN_PORT` 修改；`ANIDAN_BIND_IP` 控制宿主机监听地址。容器以 UID/GID `10001:10001` 运行。数据库位于宿主机时填写 `host.docker.internal`，数据库需监听容器可访问的接口并允许相应账号连接；数据库位于其他容器时，将服务加入共同网络并填写数据库服务名。

## 直接使用 Misaka 数据

此模式直接连接现有 MySQL / PostgreSQL 数据库，不导出、不复制数据表、不改写表结构。账号、密码散列、API 令牌、弹幕库、元数据、设置及历史记录继续存储在原库；XML、NFO、图片等文件仍使用挂载目录。原版程序仍需停止，两个服务不能同时运行写入同一套数据。

1. 备份原数据库和完整 `config` 目录，停止原版应用，保留数据库运行。
2. 在 `compose.yaml` 的 `volumes` 中添加原版完整目录的挂载（替换宿主机路径）：

   ```yaml
   - /absolute/path/to/misaka/config:/app/config
   ```

   挂载目录与其中的文件必须允许容器 UID/GID `10001:10001` 读写。自定义存储目录也需按原路径挂载，并通过配置的 `readRoots` / `writeRoots` 授予应用相应访问权限。
3. 启动 AniDan，在安装页面选择“直接接入 Misaka 旧库”。数据目录填 `/app/config`，可填写原配置文件 `/app/config/config.yml`。数据库主机留空时使用该 YAML 的连接配置；填写主机后则使用页面中的完整数据库连接参数。
4. 填写原版实际 JWT 密钥、算法和时区；YAML 中已有实际值时可留空。原版通过环境变量覆盖的设置不会回写 YAML，必须填写实际生效值。不同容器中的 `127.0.0.1` 不指向原数据库，应按新网络调整主机。
5. 保存时检查完整兼容表结构、已有用户、引用文件和目录写权限，再启动应用。登录使用原账号；不会重置管理员密码或清空旧库。

接入保留所有原表数据，但不代表所有版本或原版功能完全等价。不同版本的未知表会保留；已知表缺少字段或类型不兼容会拒绝接入，不能承诺任意最新版本旧库都可使用。动态 Python 弹幕源不能在 Go 服务中执行。当前没有使用真实用户旧库做整体验收。

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
