# Emotion

Emotion 是一个使用 Go 编写的轻量级 Emby 兼容媒体后端。它提供常用的 Emby HTTP API、本地媒体库扫描、TMDB 元数据刮削和单文件管理后台。

本项目适合希望获得类似 Emby 的 API 能力，但不想运行完整 Emby Server 的用户。

## 功能特性

- 兼容 Emby 的登录、用户、媒体库、项目、图片、播放信息、会话、收藏和已播放状态 API。
- 支持电影、剧集、分集和字幕的本地硬盘扫描与导入。
- 使用 PostgreSQL 存储数据，并支持自动数据库结构迁移。
- 默认采用 HDD 友好运行画像，降低机械硬盘上的随机 I/O 和后台探测并发。
- 支持 TMDB v3 API Key 或 v4 Bearer Token。
- 支持批量 TMDB 刷新、进度轮询和并发刮削。
- 内置管理后台，地址为 `/admin/ui`。
- 媒体列表支持 `30`、`50`、`100` 条分页。
- 支持筛选缺少海报、缺少元数据或两者任一缺失的媒体。
- 支持在管理后台手动编辑元数据。
- 支持一键刮削缺少海报或元数据的媒体。

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
| `STORAGE_TYPE` | 存储画像，`hdd` 或 `ssd`，默认 `hdd`。 |
| `MEDIA_PROBE_WORKERS` | 后台媒体信息探测默认并发，HDD 默认 `2`。 |
| `MEDIA_PROBE_MAX_WORKERS` | 后台媒体信息探测最大并发，HDD 默认 `4`。 |
| `WATCH_INTERVAL_SECONDS` | 媒体库目录监控默认轮询间隔，HDD 默认 `180`。 |
| `WATCH_MIN_INTERVAL_SECONDS` | 媒体库目录监控最小轮询间隔，HDD 默认 `60`。 |
| `TMDB_API_KEY` | 可选的 TMDB v3 Key 或 v4 Bearer Token。 |
| `TMDB_LANGUAGE` | TMDB 语言，默认 `zh-CN`。 |
| `TMDB_AUTO_SCRAPE` | 导入后是否自动刮削更新过的媒体。 |
| `EMBY_VERSION` | 返回给 Emby 客户端的版本号。 |
| `EMBY_ID` | 返回给 Emby 客户端的服务器 ID。 |

## 媒体库扫描

先在管理后台创建媒体库，然后扫描容器内已经挂载的媒体路径。

支持常见本地视频文件。目录名或文件名可以包含 TMDB 提示：

当 `STORAGE_TYPE=hdd` 时，扫描会跳过常见系统/回收目录，并在遍历阶段记录本地文件大小，减少导入阶段重复 `stat` 带来的随机磁盘访问。

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

随后轮询任务进度：

```text
GET /admin/library/scan/{job_id}?api_key=...
```

## TMDB 元数据

TMDB 元数据会存储在 PostgreSQL 的 `video_list`、`video_season`、`video_episode`、`video_image` 等表中。海报和背景图会以图片元数据形式保存，并通过兼容 Emby 的图片路由对外提供。

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

接口同时支持带 `/emby` 前缀和不带前缀的路径，以适配常见 Emby 客户端行为。

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

- 媒体库根路径会作为默认扫描路径；也可以在管理后台扫描时手动传入路径。
- 扫描速度主要受磁盘性能、文件数量、数据库延迟以及是否需要探测元数据影响。
- TMDB 刮削会受到 TMDB 速率限制影响，Emotion 会通过并发控制在尽量提高批处理速度的同时避免过度请求。
