# Emotion

Emotion 是一个使用 Go 编写的轻量级 Emby 兼容媒体后端。它提供常用的 Emby HTTP API、本地媒体库扫描、TMDB 元数据刮削、单文件管理后台，以及第三方工具常用的媒体库管理接口。

本项目适合希望获得类似 Emby 的 API 能力，但不想运行完整 Emby Server 的用户。

## 功能特性

- 兼容 Emby 的登录、用户、媒体库、项目、图片、播放信息、会话、收藏和已播放状态 API。
- 支持电影、剧集、分集、字幕和 STRM 文件的本地扫描与导入。
- 使用 PostgreSQL 存储数据，并支持自动数据库结构迁移。
- 可选 Valkey/Redis 缓存。
- 支持 TMDB v3 API Key 或 v4 Bearer Token。
- 支持批量 TMDB 刷新、进度轮询和并发刮削。
- 内置管理后台，地址为 `/admin/ui`。
- 媒体列表支持 `30`、`50`、`100` 条分页。
- 支持筛选缺少海报、缺少元数据或两者任一缺失的媒体。
- 支持在管理后台手动编辑元数据。
- 支持一键刮削缺少海报或元数据的媒体。
- 提供面向 MoviePilot 的 Emby 媒体库管理接口。
- 提供面向 MediaVault 的 Emby 媒体库管理兼容能力。

## 快速开始

```bash
git clone https://github.com/PivKeyU/Emotion.git
cd Emotion
docker compose up -d --build
```

默认服务地址：

```text
http://localhost:8096
```

管理后台地址：

```text
http://localhost:8096/admin/ui
```

Docker Compose 默认管理密钥配置在 `docker-compose.yml` 中：

```text
change-me-please
```

在公网或局域网暴露服务前，请务必修改 `API_KEY`。

## Docker Compose

仓库内置的 `docker-compose.yml` 会启动：

- `emotion`：Go 后端服务
- `postgres`：PostgreSQL 16
- `valkey`：内存缓存服务

默认情况下，本地 `./data` 会以只读方式挂载到容器内的 `/data`：

```yaml
volumes:
  - ./data:/data:ro
```

在管理后台或 API 中扫描媒体库时，请使用容器内路径，例如：

```text
/data/movies
/data/tv
```

## 配置说明

常用环境变量：

| 变量 | 说明 |
| --- | --- |
| `API_KEY` | 管理后台和第三方工具使用的管理 API Key。 |
| `SERVER_PORT` | HTTP 服务端口，默认 `8096`。 |
| `DB_DRIVER` | 数据库驱动，请使用 `postgres`。 |
| `DB_HOST` | PostgreSQL 主机地址。 |
| `DB_DATABASE` | PostgreSQL 数据库名。 |
| `DB_USERNAME` | PostgreSQL 用户名。 |
| `DB_PASSWORD` | PostgreSQL 密码。 |
| `VALKEY_HOST` | 可选的 Valkey/Redis 主机地址。 |
| `TMDB_API_KEY` | 可选的 TMDB v3 Key 或 v4 Bearer Token。 |
| `TMDB_LANGUAGE` | TMDB 语言，默认 `zh-CN`。 |
| `TMDB_AUTO_SCRAPE` | 导入后是否自动刮削更新过的媒体。 |
| `TVDB_API_KEY` | 可选的 TheTVDB v4 API Key，用于剧集保底刮削。 |
| `TVDB_PIN` | 可选的 TheTVDB PIN。 |
| `OMDB_API_KEY` | 可选的 OMDb API Key，用于 IMDb / 电影基础信息保底。 |
| `EMBY_VERSION` | 返回给 Emby 客户端的版本号。 |
| `EMBY_ID` | 返回给 Emby 客户端的服务器 ID。 |

## 媒体库扫描

先在管理后台创建媒体库，然后扫描容器内已经挂载的媒体路径。

支持常见视频文件和 STRM 文件。目录名或文件名可以包含 TMDB 提示：

```text
The Wandering Earth II (2023) [tmdb=693134]/
```

同步扫描 API 示例：

```bash
curl -X POST "http://localhost:8096/admin/library/scan?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"library_id":1,"root":"/data/movies","default_type":"movie","scrape":"on"}'
```

如果扫描耗时较长，可以使用异步扫描 API：

```bash
curl -X POST "http://localhost:8096/admin/library/scan/start?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"library_id":1,"root":"/data/movies","default_type":"movie","scrape":"on"}'
```

一键重扫所有已设置根路径的媒体库：

```bash
curl -X POST "http://localhost:8096/admin/library/scan-all/start?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"scrape":"on"}'
```

随后轮询任务进度：

```text
GET /admin/library/scan/{job_id}?api_key=...
```

## TMDB 元数据

TMDB 元数据会存储在 PostgreSQL 的 `video_list`、`video_season`、`video_episode`、`video_image` 等表中。海报和背景图会以图片元数据形式保存，并通过兼容 Emby 的图片路由对外提供。

刮削顺序会优先使用本地 NFO / 文件名中的 provider 标记（如 `[tmdb=123]`、`[imdb=tt1234567]`、`[tvdb=12345]`），再通过 TMDB 外部 ID 查找，最后回退到标题搜索。可选配置 `TVDB_API_KEY` 和 `OMDB_API_KEY` 后，剧集会使用 TheTVDB 做保底，电影和 IMDb ID 会使用 OMDb 做基础信息保底。

常用管理 API：

```text
GET  /admin/tmdb/settings
POST /admin/tmdb/settings
POST /admin/tmdb/settings/test
POST /admin/items/{id}/tmdb/refresh
POST /admin/tmdb/refresh-all
POST /admin/tmdb/refresh-all/start
GET  /admin/tmdb/refresh-all/{job_id}
```

异步批量刮削会返回以下进度信息：

- 总数量
- 已处理数量
- 剩余数量
- 匹配数量
- 跳过数量
- 失败数量

管理后台会在刮削运行时自动刷新进度。

## 管理后台媒体管理

管理后台支持：

- 分页条数选择：`30`、`50`、`100`
- 搜索
- 媒体类型筛选
- 缺少海报筛选
- 缺少元数据筛选
- 一键刮削缺失媒体
- 手动编辑元数据
- 单个媒体 TMDB 刷新

常用 API：

```text
GET   /admin/media
GET   /admin/media/stats
PATCH /admin/media/{id}
GET   /admin/media/{id}/children
```

## Emby 客户端使用

在兼容 Emby 的客户端中，将服务器地址设置为：

```text
http://<server-ip>:8096
```

可以在管理后台创建用户，也可以通过 API 创建：

```bash
curl -X POST "http://localhost:8096/Users/New?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"Name":"alice","Password":"alice123"}'
```

之后可以通过用户策略 API 或管理后台分配媒体库访问权限。

## 第三方工具兼容

Emotion 支持以下 Emby 兼容鉴权方式：

- `api_key` 查询参数
- `X-Emby-Token`
- `X-MediaBrowser-Token`
- 带有 `Token="..."` 的 `X-Emby-Authorization`

接口同时支持带 `/emby` 前缀和不带前缀的路径，以适配常见 Emby 工具的行为。

### MoviePilot

Emotion 提供 MoviePilot 风格 Emby 集成常用的媒体库管理接口：

```text
GET  /Library/SelectableMediaFolders
POST /Library/Refresh
POST /Library/Media/Updated
POST /Items/{itemId}/Refresh
```

如果 MoviePilot 需要写入或整理文件，请将同一个媒体根目录同时挂载到 MoviePilot 和 Emotion。Emotion 可以扫描相同的容器可见路径。

### MediaVault

MediaVault 可以将 Emotion 配置为 Emby 服务器，用于媒体库管理功能。

推荐的 MediaVault Emby 配置：

```text
Emby server: http://<emotion-host>:8096
API key:     Emotion 的 API_KEY 或生成的管理 API Key
UserId:      Emotion 用户 ID，例如 1
```

Emotion 已支持 MediaVault 常用于检查媒体库和媒体项目管理能力的接口：

```text
GET /System/Info
GET /Users
GET /Users/{userId}
GET /Users/{userId}/Views
GET /Users/{userId}/Items
GET /Items
GET /Items/{itemId}
GET /Library/VirtualFolders
GET /Library/SelectableMediaFolders
POST /Library/Refresh
POST /Items/{itemId}/Refresh
```

MediaVault 的 115/302 代理功能独立于 Emotion，基础媒体库管理不依赖这些功能。依赖 ScripterX、Webhook 事件或 Emby 插件专属上报的高级能力，可能还需要额外兼容工作。

## 常用 API 示例

获取服务器信息：

```bash
curl "http://localhost:8096/System/Info?api_key=change-me-please"
```

列出用户：

```bash
curl "http://localhost:8096/Users?api_key=change-me-please"
```

列出用户媒体库：

```bash
curl "http://localhost:8096/Users/1/Views?api_key=change-me-please"
```

列出媒体项目：

```bash
curl "http://localhost:8096/Users/1/Items?api_key=change-me-please&Recursive=true&Limit=50"
```

刷新媒体库：

```bash
curl -X POST "http://localhost:8096/Library/Refresh?api_key=change-me-please"
```

刷新单个媒体：

```bash
curl -X POST "http://localhost:8096/Items/vl-1/Refresh?api_key=change-me-please"
```

## 开发

运行测试：

```bash
go test ./...
```

构建 Docker 镜像：

```bash
docker compose build emotion
```

使用已有 PostgreSQL 数据库在本地运行：

```bash
cp .env.example .env
go run ./cmd/emotion
```

## 注意事项

- 媒体库根路径会作为默认扫描路径，也会用于第三方工具触发的刷新请求。如果不使用自动刷新或基于路径的扫描，可以留空并在扫描时手动传入路径。
- 媒体库支持 `is_hidden` 隐藏标记。隐藏库仍可在管理后台维护和扫描，但不会出现在普通 Emby 客户端的媒体库视图中。
- 扫描速度主要受磁盘性能、文件数量、数据库延迟以及是否需要探测元数据影响。
- TMDB 刮削会受到 TMDB 速率限制影响，Emotion 会通过并发控制在尽量提高批处理速度的同时避免过度请求。
