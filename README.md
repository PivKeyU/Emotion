# Next-Emby

> Emby 兼容的媒体 API 服务端，Go 实现
> Based on the design of [emya (emosp/emya @dev)](https://github.com/emosp/emya) and exposing the admin surface used by [Sakura_embyboss](https://github.com/berry8838/Sakura_embyboss).

Next-Emby 实现了 Emby `/emby/*` HTTP API 的一个实用子集，让任何通过 Emby API
连接的客户端（Fileball、Infuse、Hills、yamby、afusekt、femor 等）和管理工具
（Sakura_embyboss 等）能直接对接。

## 功能范围

**包含**：
- 用户认证（`AuthenticateByName`）、Token 会话
- 媒体库 / 视频列表 / 季 / 集 / 图片浏览
- 播放地址解析（302 直链，不转码）
- 字幕解析（302 直链）
- 观看进度、收藏、已播
- Sakura_embyboss 管理 API：用户增删、密码策略、媒体库可见性、使用统计

**不包含（按需求）**：
- 转码 / 直播电视 / 插件系统

## 目录结构

```
cmd/next-emby/        # main 入口
internal/
  auth/               # bcrypt 密码 + Token 生成
  cache/              # Redis/Valkey 或内存缓存
  config/             # 环境变量配置
  db/                 # MySQL 连接、schema 迁移、模型、常量
  emby/               # Emby item-id、时间格式、Tick 换算
  external/           # API_EXTERNAL 外部回调客户端
  logger/             # slog 封装
  server/             # chi 路由、中间件、handler
    ctxpkg/           # 请求上下文（userId/token/admin）
    handlers/         # 各个 Emby endpoint 的 handler
```

## 快速开始

### Docker Compose

```bash
cp .env.example .env        # 可选，compose 已内置默认值
docker compose up -d --build
# 服务会自动跑迁移，建表后监听 http://localhost:8096
```

把支持 Emby 的客户端指向 `http://<host>:8096` 即可使用。

### 本地开发

需要 Go 1.24+ 以及 MySQL 8。

```bash
cp .env.example .env
# 编辑 .env 填入 DB / API_KEY
go run ./cmd/next-emby
```

Next-Emby 启动时会自动执行 `internal/db/migrate.go` 里的建表 DDL，
无须手动 migrate。

## 本地部署完整指南

从零到能登录并播放,大致 10 分钟。

### 1. 准备依赖

- **Docker** + **Docker Compose**(推荐,最简单)
- 或者:Go 1.24+ + MySQL 8 + (可选) 一个 Emby 客户端

### 2. 克隆仓库

```bash
git clone https://github.com/PivKeyU/Next-Emby.git
cd Next-Emby
```

### 3. 准备媒体目录

在仓库根目录创建 `./media` 文件夹,把视频 / STRM 文件放进去:

```
Next-Emby/
└── media/
    ├── 电影/
    │   └── 流浪地球 2 (2023) [tmdb=693134]/
    │       ├── wandering-earth-2.mkv       # 或 .strm
    │       └── wandering-earth-2.zh.srt    # 可选字幕
    └── 剧集/
        └── 庆余年 [tmdb=136316]/
            └── Season 2/
                ├── s02e01.mkv
                └── s02e02.mkv
```

**提示**:目录/文件名里写上 `[tmdb=<id>]`,TMDB 就能精确匹配;不写也行,服务端会按 `title + year` 搜索。

### 4. 配置 .env

```bash
cp .env.example .env
```

打开 `.env`,至少改两处:

```bash
# 管理 API Key - 随便设一个长字符串
API_KEY=my-super-secret-key-change-me

# TMDB 刮削(可选但强烈推荐)
# 去 https://www.themoviedb.org/settings/api 免费申请
TMDB_API_KEY=你的tmdb密钥
```

### 5. 启动服务

```bash
docker compose up -d --build
docker compose logs -f next-emby   # 观察启动日志
```

看到这行就说明跑起来了:
```
msg="next-emby running" addr=0.0.0.0:8096
```

健康检查:
```bash
curl http://localhost:8096/emby/System/Info/Public
# {"ServerName":"next-emby","Version":"4.8.10.0","Id":"next-emby",...}
```

### 6. 创建媒体库 + 扫描入库

```bash
API=my-super-secret-key-change-me   # 和 .env 里的 API_KEY 一致

# 6.1 建一个电影库
curl -X POST "http://localhost:8096/admin/libraries?api_key=$API" \
  -H 'Content-Type: application/json' \
  -d '{"name":"电影","role":"public"}'
# 返回 {"id": 1, ...}

# 6.2 扫描(compose 把 ./media 挂到了容器内的 /data)
curl -X POST "http://localhost:8096/admin/library/scan?api_key=$API" \
  -H 'Content-Type: application/json' \
  -d '{"library_id":1,"root":"/data/电影"}'
```

返回示例(开启 TMDB 时):
```json
{
  "import": {
    "scanned_dirs": 2,
    "movies_imported": 1,
    "touched_video_list_ids": [1]
  },
  "tmdb": [
    {
      "video_list_id": 1,
      "matched_tmdb_id": "693134",
      "updated_fields": 5,
      "images_attached": 2
    }
  ]
}
```

### 7. 创建一个用户并授权

```bash
# 7.1 创建用户
curl -X POST "http://localhost:8096/emby/Users/New?api_key=$API" \
  -H 'Content-Type: application/json' \
  -d '{"Name":"alice","Password":"alice123"}'
# 返回 {"Id":"1","Name":"alice",...}

# 7.2 给用户开放电影库 (id=1)
curl -X POST "http://localhost:8096/emby/Users/1/Policy?api_key=$API" \
  -H 'Content-Type: application/json' \
  -d '{
    "IsAdministrator": false,
    "IsDisabled": false,
    "EnableContentDownloading": true,
    "EnableAllFolders": false,
    "EnabledFolders": ["vb-1"]
  }'
```

### 8. 用 Emby 客户端连接

任何 Emby 兼容客户端都行(Fileball / Infuse / yamby / afusekt / Hills / Senplayer ...):

- **服务器地址**:`http://<你的IP>:8096`
- **用户名**:`alice`
- **密码**:`alice123`

登录后应该能看到"电影"库,进去就能播放了。

### 9. 常见问题排查

**扫描没返回任何 `touched_video_list_ids`**
- 检查容器能否看到文件:`docker compose exec next-emby ls /data/电影`
- 视频文件扩展名是否在支持列表里(`.mkv .mp4 .strm .ts .avi` 等)

**TMDB 返回 `skipped: no TMDB match`**
- 目录名里加上 `[tmdb=<id>]` 绕过搜索
- 或检查 title/year 是否和 TMDB 一致

**客户端连接失败 `401 登录失效`**
- 第一次登录时必须带 `X-Emby-Authorization` header(Emby 官方客户端会自动带)
- Next-Emby 会拒绝没 Device-Id 的请求

**播放时 403**
- 视频文件读不到;检查 compose 卷挂载的权限
- `video_media.status` 必须是 `complete`,扫描器默认就这样

### 10. 不用 Docker 的本地开发

```bash
# 先启一个 MySQL
docker run -d --name ne-mysql \
  -e MYSQL_ROOT_PASSWORD=rootpw \
  -e MYSQL_DATABASE=next_emby \
  -e MYSQL_USER=next_emby \
  -e MYSQL_PASSWORD=pw \
  -p 3306:3306 \
  mysql:8 --character-set-server=utf8mb4 --collation-server=utf8mb4_unicode_ci

# 改 .env:
#   DB_HOST=127.0.0.1
#   DB_USERNAME=next_emby
#   DB_PASSWORD=pw

# 本地跑
go run ./cmd/next-emby
```

## 重要的接口约定

### Item ID

和 emya 一致，Emby ItemId 由类型前缀 + 数据库主键组成：

| 前缀  | 含义     | 对应表           |
|------|---------|-----------------|
| `vb` | 媒体库   | `library`        |
| `vl` | 视频列表 | `video_list`（电影/剧集） |
| `vs` | 季       | `video_season`   |
| `ve` | 集       | `video_episode`  |

例如 `vl-42` 是 id=42 的电影/剧集。

### 认证

客户端登录后拿到 `AccessToken`，以下三种任意一种传递都被接受：

- Header：`X-Emby-Token: <token>`
- Header：`X-Emby-Authorization: MediaBrowser Token="<token>", ...`
- Query 参数：`?api_key=<token>`

管理 API（Sakura_embyboss 调用的）直接传 `.env` 里配置的 `API_KEY`。

### 播放

- 客户端请求 `/emby/Items/{id}/PlaybackInfo`，服务端返回 `MediaSources`，
  其中 `DirectStreamUrl` 指向 `/videos/<uuid>/original.strm?api_key=...`
- 客户端请求这个直链时，服务端会 **302 重定向**到
  `video_media` 表里存的 `path_url`
- 不做转码，客户端必须支持直接播放

`path_type` 为 `url` 时直接跳转；其他类型会通过 `API_EXTERNAL` 回调外部服务拿地址
（见 emya `api.md` 定义的协议）。

## 数据导入

Next-Emby 支持**两种**入库方式:

### 方式 1:自动扫描(推荐)

准备好目录结构,调 `/admin/library/scan` 让服务端自己扫:

```bash
# 先建媒体库
curl -X POST 'http://localhost:8096/admin/libraries?api_key=YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"name":"电影库","role":"public"}'
# 返回 {"id": 1, ...}

# 扫描目录入库
curl -X POST 'http://localhost:8096/admin/library/scan?api_key=YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "library_id": 1,
    "root": "/data/movies",
    "default_type": "movie"
  }'
```

**支持的文件类型**:
- **视频**: `.mkv .mp4 .m4v .ts .avi .mov .wmv .flv .webm .iso .rmvb`
- **STRM**: `.strm` (URL 指针或本地路径,支持注释行、多备份源、BOM)
- **NFO 元数据**: Emby/Jellyfin/Kodi 标准格式
- **图片**: `poster.jpg` / `fanart.jpg` / `folder.jpg` / `backdrop.jpg` / `logo.jpg` / `thumb.jpg`
- **字幕**: `.srt .ass .vtt .ssa .sub` (与同名视频自动关联)

**识别规则**:
- `Title (2023).mkv` 或 `Title [2023].mkv` 被识别为电影
- `Show.S01E02.mkv` / `Show 1x02.mkv` / `Show E07.mkv` 被识别为剧集
- 父目录是 `Season 1` / `第一季` / `第 1 季` 时推断为剧集
- 存在 `tvshow.nfo` 时所在目录视为一部剧
- 无法识别时由 `default_type` 兜底 (`movie` 或 `tv`)

**推荐目录结构**:

```
/data/movies/
  流浪地球 2 (2023)/
    wandering-earth-2.mkv
    wandering-earth-2.nfo       # 元数据
    poster.jpg                  # 封面
    fanart.jpg                  # 背景
    wandering-earth-2.zh.srt    # 字幕(自动关联)
  Cloud Movie/
    cloud-movie.strm            # URL 指向云端
    cloud-movie.nfo

/data/tvs/
  Game of Thrones/
    tvshow.nfo                  # 整部剧的元数据
    poster.jpg
    Season 1/
      got.s01e01.mkv
      got.s01e01.nfo            # 每集元数据(可选)
      got.s01e02.mkv
```

**NFO 元数据示例** (`movie.nfo` 或 `{basename}.nfo`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<movie>
  <title>流浪地球 2</title>
  <originaltitle>The Wandering Earth II</originaltitle>
  <plot>太阳即将毁灭,人类面临流浪危机...</plot>
  <year>2023</year>
  <premiered>2023-01-22</premiered>
  <runtime>173</runtime>
  <uniqueid type="tmdb" default="true">693134</uniqueid>
  <uniqueid type="imdb">tt15302324</uniqueid>
  <genre>科幻</genre>
  <genre>动作</genre>
  <art>
    <poster>https://image.tmdb.org/t/p/w400/poster.jpg</poster>
    <fanart>https://image.tmdb.org/t/p/original/backdrop.jpg</fanart>
  </art>
</movie>
```

剧集的 NFO 用 `<tvshow>` (整部剧)、`<episodedetails>` (单集)、`<season>` (季)。
支持的标签:`title`, `originaltitle`, `plot`, `outline`, `tagline`, `year`,
`runtime`, `premiered`/`aired`/`releasedate`, `season`, `episode`, `genre`,
`tag`, `studio`, `director`, `credits`, `actor`, `uniqueid type="..."`,
`tmdbid`/`imdbid`/`tvdbid`, `thumb aspect="..."`, `fanart/thumb`,
`art/poster`, `art/fanart`。

**支持的 STRM 格式**:

```
# 单行 URL (最常见)
https://cdn.example.com/movie.mkv

# 多行带备份源和注释
# 主线路
https://primary.example.com/movie.mkv
https://fallback.example.com/movie.mkv

# 本地绝对路径
/mnt/media/movie.mkv

# 相对路径 (相对 STRM 文件所在目录)
../backup/movie.mkv

# Kodi plugin URL (透传)
plugin://plugin.video.example/play?id=123

# 云盘签名 URL (每次播放时动态解析)
https://pan.example.com/redirect?fid=abc&sign=def
```

扫描是**幂等**的 —— 重复跑同一个目录只会更新元数据,不会重复入库。

### 方式 2:直接写 MySQL

如果有自己的爬虫/刮削流水线,可以绕开扫描器直接写表。表结构:

1. `library` —— 媒体库
2. `video_list` —— 每部电影/剧
3. `video_season` + `video_episode` —— 剧集的季/集
4. `video_media` —— 播放地址 (`path_type='url'`/`'local'` + `path_url`)
5. `video_subtitle` —— 可选字幕
6. `video_image` —— 封面/剧照 (`type='Primary'/'Backdrop'/...`)

然后通过管理 API 创建用户 + 分配可见媒体库即可。

## 管理 API 速查

| 端点 | 用途 |
|------|------|
| `GET  /admin/libraries` | 列出所有媒体库 |
| `POST /admin/libraries` | 创建媒体库 `{"name":"...","role":"..."}` |
| `DELETE /admin/libraries/{id}` | 软删除媒体库 |
| `POST /admin/library/scan` | 扫描目录入库(见上) |
| `POST /admin/items/{id}/tmdb/refresh` | 单个条目重新刮削 TMDB |
| `POST /admin/tmdb/refresh-all` | 批量刮削缺失元数据 |

## TMDB 自动刮削

Next-Emby 内置了 TMDB 客户端,可以自动给入库的条目填充标题、简介、海报、背景图、首映日、时长、类型等元数据。默认语言为中文(zh-CN)。

### 开启方式

1. 在 [themoviedb.org/settings/api](https://www.themoviedb.org/settings/api) 免费申请 API Key
2. 在 `.env` 里填 `TMDB_API_KEY=<你的 key>`(v3 API Key 或 v4 Bearer Token 都行)
3. `TMDB_AUTO_SCRAPE=true`(默认)时,每次 `/admin/library/scan` 会自动对入库条目发起刮削

### 匹配策略

按下面这个顺序找 TMDB id:

1. 目录或文件名里的 `[tmdb=502419]` 标注(Emby/Jellyfin 通用约定)
2. NFO 里的 `<uniqueid type="tmdb">` 或 `<tmdbid>`
3. 数据库里已有的 `tmdb_id` 字段
4. 按 `title + year` 调用 `/search/movie` 或 `/search/tv`,取第一个命中

### 更新规则

**默认不覆盖已有数据** —— 只填空字段。这意味着你手工写过的简介、改过的标题不会被 TMDB 盖掉。
如果想强制覆盖,调用时传 `{"force": true}`,或用 `POST /admin/tmdb/refresh-all`。

### 举例

```bash
# 扫描时顺便刮削(默认就是这个行为)
curl -X POST 'http://localhost:8096/admin/library/scan?api_key=YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"library_id":1,"root":"/data/movies","scrape":"on"}'

# 只刮削,不扫描(补缺失的元数据)
curl -X POST 'http://localhost:8096/admin/tmdb/refresh-all?api_key=YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"max":200,"force":false}'

# 强制刷新一个条目
curl -X POST 'http://localhost:8096/admin/items/42/tmdb/refresh?api_key=YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"force":true}'
```

对于 `A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]` 这种目录名,
扫描器会自动提取出 `tmdb=502419`,直接去 TMDB 拿元数据,不用猜题名。

## Sakura_embyboss 集成

在 Sakura_embyboss 的 `config.json` 里填：

```jsonc
{
  "emby_url":  "http://next-emby:8096",
  "emby_api":  "<你在 .env 里配置的 API_KEY>"
}
```

Sakura_embyboss 使用的所有 Emby endpoint 均已实现：

| 端点 | 用途 |
|------|------|
| `POST /emby/Users/New` | 创建用户 |
| `DELETE /emby/Users/{id}` | 删除用户 |
| `POST /emby/Users/{id}/Password` | 设置/重置密码 |
| `POST /emby/Users/{id}/Policy` | 管理员/禁用/可见库 |
| `GET /emby/Users` / `GET /emby/Users/Query` | 列表/搜索 |
| `GET /emby/Library/VirtualFolders` | 列媒体库 |
| `GET /emby/Sessions` | 当前会话 |
| `POST /emby/Sessions/{id}/Message` | 向设备发消息（no-op） |
| `POST /emby/Sessions/{id}/Playing/Stop` | 终止会话 |
| `GET /emby/Devices/Info` | 查询设备 |
| `POST /emby/user_usage_stats/submit_custom_query` | 使用统计（识别 Sakura 的两条固定 SQL） |

## License

MIT，源自 emya 项目的接口设计。
