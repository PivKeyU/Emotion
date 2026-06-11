package handlers

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/importer"
)

// mediaProbeCoordinator runs an on-demand ffprobe during PlaybackInfo for
// remote-URL media that is missing duration/container/streams metadata. It
// keeps the synchronous wait bounded (probeDeadline) and uses singleflight +
// a negative cache to make sure no media is probed twice in a short window.
type mediaProbeCoordinator struct {
	db       *db.DB
	log      *slog.Logger
	group    singleflight.Group
	failures sync.Map // key: media id (string), value: time.Time of last failure
}

const (
	probeDeadline   = 5 * time.Second
	probeFailureTTL = 10 * time.Minute
)

func newMediaProbeCoordinator(database *db.DB, log *slog.Logger) *mediaProbeCoordinator {
	return &mediaProbeCoordinator{db: database, log: log}
}

// ProbeIfMissing inspects each media row and synchronously probes any remote
// URL media that is missing metadata. Results are written back to the DB and
// applied in-place so the caller can build MediaSources from the same slice.
// All errors are swallowed: playback must continue with whatever data we have.
func (c *mediaProbeCoordinator) ProbeIfMissing(ctx context.Context, medias []videoMediaRow) []videoMediaRow {
	if c == nil {
		return medias
	}
	for i := range medias {
		if !shouldProbeForPlayback(medias[i]) {
			continue
		}
		if c.recentFailure(medias[i].ID) {
			continue
		}
		info := c.probeOnce(ctx, medias[i])
		if info == nil {
			continue
		}
		c.applyProbe(&medias[i], info)
	}
	return medias
}

func shouldProbeForPlayback(m videoMediaRow) bool {
	if strings.ToLower(strings.TrimSpace(m.PathType)) != "url" {
		return false
	}
	if strings.TrimSpace(m.PathURL) == "" {
		return false
	}
	if strings.TrimSpace(m.FileMetadata) != "" && m.FileSecond > 0 && strings.TrimSpace(m.FileContainer) != "" {
		return false
	}
	return true
}

func (c *mediaProbeCoordinator) recentFailure(id int64) bool {
	v, ok := c.failures.Load(id)
	if !ok {
		return false
	}
	t, ok := v.(time.Time)
	if !ok {
		return false
	}
	if time.Since(t) > probeFailureTTL {
		c.failures.Delete(id)
		return false
	}
	return true
}

// probeOnce executes one ffprobe call per media id at a time. Concurrent
// callers piggy-back on the same in-flight result.
func (c *mediaProbeCoordinator) probeOnce(ctx context.Context, m videoMediaRow) *importer.MediaProbeInfo {
	key := strconv.FormatInt(m.ID, 10)
	v, err, _ := c.group.Do(key, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeDeadline)
		defer cancel()
		info, err := importer.ProbeLocalMedia(probeCtx, m.PathURL)
		if err != nil {
			c.failures.Store(m.ID, time.Now())
			if c.log != nil {
				c.log.Warn("playback probe failed", "category", "playback", "media_id", m.ID, "err", err)
			}
			return nil, err
		}
		c.writeBack(probeCtx, m.ID, m.PathURL, info)
		return info, nil
	})
	if err != nil || v == nil {
		return nil
	}
	info, _ := v.(*importer.MediaProbeInfo)
	return info
}

func (c *mediaProbeCoordinator) writeBack(ctx context.Context, mediaID int64, path string, info *importer.MediaProbeInfo) {
	if c.db == nil {
		return
	}
	var fileSize sql.NullInt64
	if !strings.HasPrefix(strings.ToLower(path), "http://") && !strings.HasPrefix(strings.ToLower(path), "https://") {
		if stat, err := os.Stat(path); err == nil && stat.Size() > 0 {
			fileSize = sql.NullInt64{Valid: true, Int64: stat.Size()}
		}
	}
	if !fileSize.Valid && info.Size > 0 {
		fileSize = sql.NullInt64{Valid: true, Int64: info.Size}
	}
	_, err := c.db.ExecContext(ctx, `
		UPDATE video_media
		SET file_size = COALESCE(?, file_size),
		    file_second = ?, file_matadata = ?, file_container = ?, updated_at = NOW()
		WHERE id = ?
	`,
		fileSize,
		sql.NullInt64{Valid: info.Duration > 0, Int64: info.Duration},
		sql.NullString{Valid: len(info.Metadata) > 0, String: string(info.Metadata)},
		nullableString(info.Container),
		mediaID,
	)
	if err != nil && c.log != nil {
		c.log.Warn("playback probe write-back failed", "category", "playback", "media_id", mediaID, "err", err)
	}
}

func (c *mediaProbeCoordinator) applyProbe(m *videoMediaRow, info *importer.MediaProbeInfo) {
	if len(info.Metadata) > 0 {
		m.FileMetadata = string(info.Metadata)
	}
	if info.Duration > 0 {
		m.FileSecond = info.Duration
	}
	if info.Container != "" {
		m.FileContainer = info.Container
	}
	if info.Size > 0 && m.FileSize == 0 {
		m.FileSize = info.Size
	}
}
