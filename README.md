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

前期建议直接写 MySQL。核心数据流：

1. `library` —— 媒体库
2. `video_list` —— 每部电影/剧
3. `video_season` + `video_episode` —— 剧集的季/集
4. `video_media` —— 播放地址（`path_type='url'` + `path_url='https://...'`）
5. `video_subtitle` —— 可选字幕
6. `video_image` —— 封面/剧照（`type='Primary'/'Backdrop'/...`）

然后通过管理 API 创建用户 + 分配可见媒体库即可。

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
