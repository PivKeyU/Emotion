# Emotion

Emotion is a lightweight Emby-compatible media backend written in Go. It provides
common Emby HTTP APIs, local media library scanning, TMDB metadata scraping, a
single-file admin dashboard, and management endpoints used by third-party tools.

The project is designed for users who want an Emby-like API surface without
running the full Emby server.

## Features

- Emby-compatible login, users, libraries, items, images, playback info,
  sessions, favorites and played-state APIs.
- Local media scan and import for movies, TV shows, episodes, subtitles and
  STRM files.
- PostgreSQL storage with automatic schema migration.
- Optional Valkey/Redis cache.
- TMDB scraping with v3 API key or v4 bearer token support.
- Batch TMDB refresh with progress polling and parallel workers.
- Admin dashboard at `/admin/ui`.
- Media list pagination with page sizes `30`, `50`, and `100`.
- Filters for media missing posters, metadata, or either.
- Manual metadata editing from the admin dashboard.
- One-click scraping for media missing posters or metadata.
- MoviePilot-oriented Emby management endpoints.
- MediaVault-oriented Emby media library management compatibility.

## Quick Start

```bash
git clone https://github.com/PivKeyU/Emotion.git
cd Emotion
docker compose up -d --build
```

The default service URL is:

```text
http://localhost:8096
```

Open the admin dashboard:

```text
http://localhost:8096/admin/ui
```

The default Docker Compose admin key is configured in `docker-compose.yml`:

```text
change-me-please
```

Change `API_KEY` before exposing the service.

## Docker Compose

The included `docker-compose.yml` starts:

- `emotion`: the Go backend
- `postgres`: PostgreSQL 16
- `valkey`: in-memory cache

By default, local `./data` is mounted into the container as `/data`:

```yaml
volumes:
  - ./data:/data:ro
```

When scanning a library from the admin UI or API, use the container path, for
example:

```text
/data/movies
/data/tv
```

## Configuration

Important environment variables:

| Variable | Description |
| --- | --- |
| `API_KEY` | Admin API key used by the dashboard and third-party tools. |
| `SERVER_PORT` | HTTP port, default `8096`. |
| `DB_DRIVER` | Database driver. Use `postgres`. |
| `DB_HOST` | PostgreSQL host. |
| `DB_DATABASE` | PostgreSQL database name. |
| `DB_USERNAME` | PostgreSQL user. |
| `DB_PASSWORD` | PostgreSQL password. |
| `VALKEY_HOST` | Optional Valkey/Redis host. |
| `TMDB_API_KEY` | Optional TMDB v3 key or v4 bearer token. |
| `TMDB_LANGUAGE` | TMDB language, default `zh-CN`. |
| `TMDB_AUTO_SCRAPE` | Automatically scrape touched items after import. |
| `EMBY_VERSION` | Version string returned to Emby clients. |
| `EMBY_ID` | Server ID returned to Emby clients. |

## Media Library Scanning

Create a media library in the admin dashboard, then scan a path mounted inside
the container.

Supported media extensions include common video files and STRM files. Folder or
file names can include a TMDB hint:

```text
The Wandering Earth II (2023) [tmdb=693134]/
```

Example API call:

```bash
curl -X POST "http://localhost:8096/admin/library/scan?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"library_id":1,"root":"/data/movies","default_type":"movie","scrape":"on"}'
```

For long scans, use the async API:

```bash
curl -X POST "http://localhost:8096/admin/library/scan/start?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"library_id":1,"root":"/data/movies","default_type":"movie","scrape":"on"}'
```

Then poll:

```text
GET /admin/library/scan/{job_id}?api_key=...
```

## TMDB Metadata

TMDB metadata is stored in PostgreSQL tables such as `video_list`,
`video_season`, `video_episode`, and `video_image`. Poster and backdrop image
records are stored as image metadata and served through Emby-compatible image
routes.

Common admin APIs:

```text
GET  /admin/tmdb/settings
POST /admin/tmdb/settings
POST /admin/tmdb/settings/test
POST /admin/items/{id}/tmdb/refresh
POST /admin/tmdb/refresh-all
POST /admin/tmdb/refresh-all/start
GET  /admin/tmdb/refresh-all/{job_id}
```

The async batch scraper reports:

- total items
- processed items
- remaining items
- matched items
- skipped items
- failed items

The admin UI refreshes this progress while scraping is running.

## Admin Media Management

The admin dashboard supports:

- Page size selection: `30`, `50`, `100`
- Search
- Type filtering
- Missing poster filtering
- Missing metadata filtering
- One-click scrape missing media
- Manual metadata editing
- Per-item TMDB refresh

Useful APIs:

```text
GET   /admin/media
GET   /admin/media/stats
PATCH /admin/media/{id}
GET   /admin/media/{id}/children
```

## Emby Client Usage

Use Emotion as the server address in an Emby-compatible client:

```text
http://<server-ip>:8096
```

Users can be created from the admin dashboard or by API:

```bash
curl -X POST "http://localhost:8096/Users/New?api_key=change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"Name":"alice","Password":"alice123"}'
```

Then assign library access through the user policy API or admin UI.

## Third-Party Tool Compatibility

Emotion accepts Emby-compatible API tokens in:

- `api_key` query parameter
- `X-Emby-Token`
- `X-MediaBrowser-Token`
- `X-Emby-Authorization` with `Token="..."`

Routes are available both with and without the `/emby` prefix, matching common
Emby tooling behavior.

### MoviePilot

Emotion includes common library management APIs used by MoviePilot-style Emby
integrations:

```text
GET  /Library/SelectableMediaFolders
POST /Library/Refresh
POST /Library/Media/Updated
POST /Items/{itemId}/Refresh
```

Mount the same media root into MoviePilot and Emotion if MoviePilot needs to
write or organize files. Emotion can then scan the same container-visible path.

### MediaVault

MediaVault can be configured to use Emotion as its Emby server for media library
management features.

Recommended MediaVault Emby settings:

```text
Emby server: http://<emotion-host>:8096
API key:     your Emotion API_KEY or generated admin API key
UserId:      an Emotion user id, for example 1
```

Emotion now supports the common APIs MediaVault checks for library and item
management:

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

MediaVault's 115/302 proxy features are separate from Emotion and are not
required for basic media library management. Advanced MediaVault features that
depend on ScripterX, webhook events, or Emby plugin-specific reporting may
require additional compatibility work.

## Common API Examples

Server info:

```bash
curl "http://localhost:8096/System/Info?api_key=change-me-please"
```

List users:

```bash
curl "http://localhost:8096/Users?api_key=change-me-please"
```

List user libraries:

```bash
curl "http://localhost:8096/Users/1/Views?api_key=change-me-please"
```

List items:

```bash
curl "http://localhost:8096/Users/1/Items?api_key=change-me-please&Recursive=true&Limit=50"
```

Refresh library:

```bash
curl -X POST "http://localhost:8096/Library/Refresh?api_key=change-me-please"
```

Refresh one item:

```bash
curl -X POST "http://localhost:8096/Items/vl-1/Refresh?api_key=change-me-please"
```

## Development

Run tests:

```bash
go test ./...
```

Build Docker image:

```bash
docker compose build emotion
```

Run locally with an existing PostgreSQL database:

```bash
cp .env.example .env
go run ./cmd/emotion
```

## Notes

- The library root path is used as the default scan path and for third-party
  refresh requests. If you never use automatic refresh or path-based scanning,
  it can be left empty and scan paths can be provided manually.
- Scanning speed depends mostly on disk performance, number of files, database
  latency, and whether metadata probing is required.
- TMDB scraping speed is rate-limited and parallelized to avoid overloading TMDB
  while still processing batches quickly.
