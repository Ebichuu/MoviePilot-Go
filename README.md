# MoviePilot Go

> **来源声明：本项目源自 [MoviePilot](https://github.com/jxxghp/MoviePilot)（作者 jxxghp）。**
> 它是对原项目 `v2.15.6` 的 Go 重写，目标是保持原版的功能与业务行为；接口契约、数据结构、
> 站点/元数据/通知等业务语义均来自原项目。原项目为 Python 实现、以 GPL-3.0 发布，本项目沿用
> 同一许可证；上游源码只作为对照基线，**不随本仓库分发**。Web 前端产物来自
> [MoviePilot-Frontend](https://github.com/jxxghp/MoviePilot-Frontend)。
> 原项目及其作者的著作权归其所有，本仓库不主张。合规细节见 [NOTICE](NOTICE)。

后端是纯 Go 的模块化单体：HTTP 层兼容 MoviePilot V2 的接口契约，官方 Vue 前端发布包原样托管，
数据落在 PostgreSQL，后台任务走 River 队列，Redis 只作可选缓存。

## 技术栈

| 组件 | 说明 |
| --- | --- |
| Go | 1.26，单二进制，无 CGO 依赖 |
| PostgreSQL | 唯一数据源（开发/CI 用 17.6），Schema 由 Goose 迁移管理 |
| River | 基于 PostgreSQL 的任务队列：订阅搜索、RSS 轮次、下载对账、整理、NFO、通知等 |
| Redis | 可选缓存与元数据 TTL，未配置时以无缓存模式启动 |
| 前端 | MoviePilot-Frontend `v2.15.6` 构建产物，由镜像构建时下载并按 sha256 校验 |

## 功能范围

**已实现（主链路闭环）**

- 订阅：电影/电视剧创建、编辑、重置、精准搜索、缺集与**完整多档位洗版规则**（支持内置 `2XFREE`/`H50`/`H30`/`!HR`/`AV1`/`DV`/`HDR10P`/`IMAX` 等 V2 规则、内置默认洗版梯度与规则测试 `/system/ruletest`）、完成后状态收敛、Overseerr/Jellyseerr Webhook 自动订阅（`/api/v1/subscribe/seerr`）
- 站点：8 个内置站点适配器 + 通用 **NexusPHP** 适配器 + **Torznab / Jackett / Prowlarr** 聚合协议，站点凭据用主密钥加密落库，支持种子缓存查询/刷新/删除/重新识别（`/api/v1/torrent/cache`）
- 下载：qBittorrent / Transmission / rTorrent 提交、名称与大小接管、结果未知的幂等对账、进度查询
- 整理：不可变计划 + 逐文件 CAS + SHA-256 校验，支持 V2 自定义 Jinja2 命名模板（`MOVIE_RENAME_FORMAT` / `TV_RENAME_FORMAT`）、本地目录监控（`monitor_type: "monitor"`）、手动刮削（`/api/v1/media/scrape/{storage}`）、手动多模式与**跨存储流式整理**（Local / SMB / 阿里云盘 / 115 / Alist 互传）、实时整理队列/进度（`/transfer/queue`、`/transfer/now`、`/system/progress/{process_type}`）、整季集号格式推荐（`/transfer/episode-format/recommend`）与失败历史记录查询/清理
- 媒体库：**Emby / Jellyfin / Plex** 入库确认、条目同步缓存、首页卡片数据（媒体库列表/图片/跳转链接、最近入库 `/mediaserver/latest`、继续观看 `/mediaserver/playing`）、NFO 与图片刮削、刷新请求、播放链接生成、Emby/Jellyfin/Plex Webhook 接收通知（`/api/v1/webhook/`）
- 通知：Telegram（含入站 Bot）、Discord、飞书、Slack、SynologyChat、VoceChat、QQ、企业微信应用、WebPush，支持 V2 默认模板、逐目标重试与漏投恢复
- 存储与扩展：本地存储、Rclone、Alist、**SMB**（纯 Go `go-smb2`）、**阿里云盘**（PKCE 扫码登录 + Open API）、**115 网盘**（PKCE 扫码 / OAuth2 + Open API）；5 个 Go 原生内置插件（站点签到、自动备份、RSS 增强、订阅助手、媒体库同步）；CookieCloud 同步；**MFA/OTP 与 Passkey / WebAuthn**（支持注册、列表、密码确认删除与无密码/用户名登录）
- 字幕：支持 **OpenSubtitles REST API v1** 与 **Assrt（伪射手）** 并发搜索（普通 + SSE 流式 `/api/v1/search/subtitle/...`）与安全下载/ZIP 自动解压（`/api/v1/download/subtitle`）
- 元数据与发现：TMDB、豆瓣、**TheTVDB v4**（剧集/单集补全）、**Fanart.tv v3**（高清 Logo/ClearArt/Banner/Thumb/Disc/季海报刮削）、Bangumi、AniList；发现/推荐页直接用这些来源
- 运维：整库备份/恢复、River 周期任务投影到 `/dashboard/schedule` + `/system/runscheduler` 全量常用任务映射、宿主机/进程监控 `/dashboard/processes`、GitHub Releases 版本列表 `/system/versions`（带缓存）、Web 端内存实时日志查看/下载（`/system/logging` SSE）、`/healthz` 与 `/readyz`、结构化日志、登录防爆破
- 迁移：从 V2 的 SQLite 单命令导入
- 配置：`/api/v1/system/env` 在线修改并持久化到 `app.env`

**尚未实现（不提供占位实现）**

- Python 插件运行时（V2 插件体系）与插件市场
- 工作流引擎、Agent / LLM / MCP 相关能力
- 国产 NAS 影视墙（飞牛/绿联/极空间）
- 企业微信 ClawBot
- 部分站点辅助接口（如站点分类）仍返回 V2 兼容空状态

## 快速开始

### 用镜像（推荐）

```powershell
Copy-Item .env.example .env      # 按注释填写 POSTGRES_PASSWORD、MOVIEPILOT_IMAGE 等
docker compose pull
docker compose up -d
```

生产 Compose 不构建镜像，`MOVIEPILOT_IMAGE` 缺失时会直接失败；请使用发布说明里的
`ghcr.io/...@sha256:...` digest，回滚时改回上一份已验收的 digest。首次启动自动跑迁移，
然后访问 `http://localhost:3000`。

### 本地开发

```powershell
.\scripts\dev\up.ps1            # 构建并后台启动 postgres + redis + 应用
.\scripts\dev\up.ps1 -Logs      # 前台启动，直接看日志
.\scripts\dev\up.ps1 -Down      # 停止（保留数据卷）
```

默认管理员 `admin` / `moviepilot-dev`，可用 `-BootstrapPassword` 覆盖；改了 Go 代码加
`-Recreate` 重建。也可以直接跑源码：

```powershell
$env:MOVIEPILOT_DATABASE_URL='postgres://moviepilot:<密码>@127.0.0.1:5432/moviepilot?sslmode=disable'
go run ./cmd/moviepilot-go
```

未配置外部服务时程序仍能启动，`/readyz` 会明确报告依赖未就绪。

## 配置

配置优先级：**进程环境变量 > `CONFIG_DIR/app.env`（默认 `config/app.env`）> 内置默认值**。
`KEY=` 这种空值视为未设置，不会遮住文件里的值；界面里可写的配置项同样写回这个文件，
所以 `.env` 风格部署只需维护一份 `config/app.env`。

| 变量 | 说明 |
| --- | --- |
| `MOVIEPILOT_DATABASE_URL` | PostgreSQL DSN，必需 |
| `MOVIEPILOT_MASTER_KEY` | 敏感配置加密主密钥（≥16 字节）。**首次录入站点凭据后不可更换**，换了所有站点 Cookie/API Key 都解不开 |
| `MOVIEPILOT_JWT_SECRET` | 生产必需；开发环境缺省时用进程级临时密钥，重启后旧令牌失效 |
| `MOVIEPILOT_RESOURCE_SECRET` | 生产必需的资源下载令牌密钥 |
| `MOVIEPILOT_HTTP_ADDR` | 监听地址，默认 `:3000` |
| `MOVIEPILOT_FRONTEND_DIR` | 前端静态目录，默认 `web/dist` |
| `MOVIEPILOT_REDIS_URL` | 可选，Redis 缓存 |
| `MOVIEPILOT_TMDB_TOKEN` / `MOVIEPILOT_DOUBAN_API_KEY` / `MOVIEPILOT_DOUBAN_SECRET_KEY` | 元数据来源；Bangumi、AniList 免配置 |
| `MOVIEPILOT_TVDB_API_KEY` / `MOVIEPILOT_TVDB_API_PIN` / `MOVIEPILOT_FANART_API_KEY` / `MOVIEPILOT_FANART_LANG` | 可选，TheTVDB v4 与 Fanart.tv v3 刮削/艺术图补全配置（默认语言 `zh,en`） |
| `MOVIEPILOT_STORAGE_SMB_*` / `MOVIEPILOT_STORAGE_ALIPAN_*` / `MOVIEPILOT_STORAGE_U115_*` | 可选，SMB / 阿里云盘 / 115 网盘存储配置（也支持在 Web 端在线配置或扫码登录并加密落库） |
| `MOVIEPILOT_OPENSUBTITLES_API_KEY` / `MOVIEPILOT_ASSRT_TOKEN` / `MOVIEPILOT_SUBTITLE_LANGS` | 可选，字幕搜索与下载源配置（默认语言 `zh-cn,zh-tw,en`） |
| `MOVIEPILOT_LOGIN_MAX_FAILURES` / `MOVIEPILOT_LOGIN_LOCK_*` | 登录锁定阈值与退避上限 |
| `MOVIEPILOT_BACKUP_TEMP_DIR` | 备份临时目录，默认系统临时目录 |
| `MOVIEPILOT_TIMEZONE` | V2 时间字符串时区，默认 `Asia/Shanghai` |

站点 `config` 里的 `cookie`、`apikey`、`passkey`、`token`、`secret` 等不允许明文写入，
由主密钥加密到 `sites.secrets`；编辑时未提交或为 `null` 的敏感字段保留旧值。

## 备份、恢复与从 V2 迁移

```powershell
# 备份：管理员会话下导出整库 tar.gz
curl.exe -H "Authorization: Bearer <token>" -o backup.tar.gz http://localhost:3000/api/v1/system/backup

# 恢复：只恢复两边都存在的表，必须显式确认
curl.exe -X POST -H "Authorization: Bearer <token>" --data-binary "@backup.tar.gz" `
  "http://localhost:3000/api/v1/system/restore?confirm=true"
```

备份走 PostgreSQL `COPY` 流式导出，包含 `manifest.json`（表清单、Schema 版本、导出时间、
`app_version`），不含表结构；跳过 River 运行时队列表。恢复在单个事务里按外键拓扑
`TRUNCATE ... RESTART IDENTITY CASCADE` 后导入并修正序列。

从 V2（SQLite）迁过来用自带子命令，先只读试运行：

```bash
moviepilot-go migrate-v2 --db /path/to/moviepilot.db --dry-run   # 打印每张表的映射结果
moviepilot-go migrate-v2 --db /path/to/moviepilot.db             # 写库前自动整库备份
```

导入用户（bcrypt 摘要兼容，原密码可用）、系统设置、订阅、站点用户数据与站点统计；
重复执行幂等，源库始终只读。订阅默认导入为 **暂停**，确认无误后再用 `--resume` 或手工恢复，
避免迁移完立刻批量下载。V2 站点是 Python 插件实例，不自动导入，工具会打印清单提示按适配器重建。

## 登录与安全

登录入口 `POST /api/v1/login/access-token` 带防爆破：同一用户名连续失败 5 次进入指数退避锁定
（1 分钟起，上限 15 分钟），同一来源 IP 10 分钟内失败 20 次同样锁定，成功登录清零；
被锁返回 `429` 与 `Retry-After`。`X-Forwarded-For` 只在直连对端是回环/内网地址时才采信。
限流状态在进程内，多副本部署需要换成 Redis。

## 版本

版本号由构建期注入，不写死在源码里：

```powershell
go build -ldflags "-X github.com/moviepilot-go/moviepilot-go/internal/version.Version=v1.2.3 -X github.com/moviepilot-go/moviepilot-go/internal/version.Commit=a1b2c3d -X github.com/moviepilot-go/moviepilot-go/internal/version.BuildDate=2026-11-20T14:00:00Z" -o moviepilot-go ./cmd/moviepilot-go
.\moviepilot-go version      # v1.2.3 (a1b2c3d, 2026-11-20T14:00:00Z)
```

`version` 子命令不读配置，配置文件坏了也能查版本；未注入时输出 `dev`。镜像构建接受
`--build-arg VERSION/COMMIT/BUILD_DATE`，启动日志和备份 manifest 用的是同一个值。

## 升级与回滚

1. **先备份**：管理端导出整库包，或直接备份 PostgreSQL 数据卷。
2. **换镜像**：改 `.env` 里的 `MOVIEPILOT_IMAGE` 为新 digest，`docker compose pull && docker compose up -d`；迁移失败会退出而不是半启动。
3. **别换密钥**：`MOVIEPILOT_MASTER_KEY`、`MOVIEPILOT_JWT_SECRET`、`MOVIEPILOT_RESOURCE_SECRET` 必须前后一致。
4. **回滚**：把 digest 改回去即可；迁移是单向的，旧版本读不了新 Schema 时用第 1 步的备份恢复。

## 开发与校验

```powershell
go build ./...
go test ./...                                  # 单元与契约测试
go test -tags integration ./internal/database  # 需 MOVIEPILOT_TEST_DATABASE_URL
.\scripts\run-postgres-integration.ps1         # 起测试库并跑集成测试
```

架构约束由 `internal/architecture` 的测试强制：`database/sql` 只能由 `internal/database`
导入，Goose 归 `internal/database`，River 归 `internal/jobs`，go-redis 归 `internal/cache`，
`httpapi` 只由 `internal/app` 组装。Linux CI 额外跑 `CGO_ENABLED=1 go test -race`，
Windows 工作站为 `CGO_ENABLED=0`，本机不能跑 race detector。

## 许可与致谢

本项目以 **GPL-3.0** 发布（见 [LICENSE](LICENSE)），与上游
[MoviePilot](https://github.com/jxxghp/MoviePilot)（jxxghp，GPL-3.0）保持一致；随包分发的
`web/dist` 是 MoviePilot-Frontend `v2.15.6`（同为 GPL-3.0）的构建产物，第三方组件与来源见
[NOTICE](NOTICE)。分发二进制时按 GPL-3.0 第 6 条一并提供源码或获取途径。

感谢 MoviePilot 作者 jxxghp 与所有原项目贡献者。
