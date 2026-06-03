package importer

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PivKeyU/Emotion/internal/db"
)

// Options tune an import run.
type Options struct {
	// LibraryID is the target library.id. Required.
	LibraryID int64
	// Root is the directory to scan. Must exist.
	Root string
	// DefaultType decides how loose files (no NFO, no SxxExx) are classified:
	// "movie" or "tv". When empty, the importer infers per-directory from
	// folder structure (season folders => tv).
	DefaultType string
	// FollowSymlinks passes through to Scan.
	FollowSymlinks bool
	// DryRun prevents any DB writes; still produces a Report.
	DryRun bool
	// ProbeMedia runs ffprobe while importing. This is slower and should be
	// reserved for small libraries or explicit rescans; large libraries can use
	// the admin media probe job after fast import.
	ProbeMedia bool
	// Logger receives progress messages.
	Logger *slog.Logger
	// Progress receives coarse scanner/importer progress updates.
	Progress func(Progress)
}

// Report captures outcomes for a user-visible summary.
type Report struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   int64     `json:"duration_ms"`
	Scanned    int       `json:"scanned_dirs"`
	Movies     int       `json:"movies_imported"`
	Series     int       `json:"series_imported"`
	Seasons    int       `json:"seasons_imported"`
	Episodes   int       `json:"episodes_imported"`
	MediaRows  int       `json:"media_rows_imported"`
	Skipped    int       `json:"skipped"`
	Errors     []string  `json:"errors,omitempty"`

	// TouchedVideoListIDs is the set of video_list rows that were inserted or
	// updated during this run. Callers (typically the admin handler) can
	// forward these to a TMDB scraper for post-import metadata backfill.
	TouchedVideoListIDs []int64 `json:"touched_video_list_ids,omitempty"`
}

// Progress is a snapshot of the current import run.
type Progress struct {
	Stage      string `json:"stage"`
	Current    string `json:"current,omitempty"`
	WalkedDirs int    `json:"walked_dirs,omitempty"`
	Processed  int    `json:"processed_dirs,omitempty"`
	Total      int    `json:"total_dirs,omitempty"`
	Report     Report `json:"report"`
}

// Importer coordinates scanning and DB upserts.
type Importer struct {
	db  *db.DB
	log *slog.Logger
}

// New constructs an Importer.
func New(database *db.DB, log *slog.Logger) *Importer {
	return &Importer{db: database, log: log}
}

// Run executes a single import.
func (i *Importer) Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Root == "" {
		return nil, errors.New("root is required")
	}
	if opts.LibraryID <= 0 {
		return nil, errors.New("library_id is required")
	}
	log := opts.Logger
	if log == nil {
		log = i.log
	}

	// Validate library exists.
	var libraryExists int64
	if err := i.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM library WHERE id = ? AND deleted_at IS NULL", opts.LibraryID,
	).Scan(&libraryExists); err != nil || libraryExists == 0 {
		return nil, fmt.Errorf("library %d not found", opts.LibraryID)
	}

	rep := &Report{StartedAt: time.Now()}
	lastProgressAt := time.Time{}
	lastWalked := 0
	lastProcessed := 0
	emitProgress := func(stage, current string, walked, processed, total int) {
		if opts.Progress == nil {
			return
		}
		now := time.Now()
		force := stage == "queued" || stage == "walking" && walked <= 1 ||
			stage == "importing" && processed == 0 || stage == "done" ||
			stage == "failed" || stage == "canceled"
		if !force {
			if stage == "walking" && walked > 0 && walked-lastWalked < 100 && now.Sub(lastProgressAt) < 500*time.Millisecond {
				return
			}
			if stage == "importing" && processed > 0 && processed-lastProcessed < 25 && now.Sub(lastProgressAt) < 500*time.Millisecond {
				return
			}
		}
		lastProgressAt = now
		if walked > 0 {
			lastWalked = walked
		}
		if processed > 0 {
			lastProcessed = processed
		}
		snap := *rep
		snap.Errors = append([]string(nil), rep.Errors...)
		snap.TouchedVideoListIDs = append([]int64(nil), rep.TouchedVideoListIDs...)
		opts.Progress(Progress{
			Stage:      stage,
			Current:    current,
			WalkedDirs: walked,
			Processed:  processed,
			Total:      total,
			Report:     snap,
		})
	}
	emitProgress("walking", opts.Root, 0, 0, 0)

	dirs, err := Scan(ScanOptions{
		Root:           opts.Root,
		FollowSymlinks: opts.FollowSymlinks,
		OnDir: func(path string, seen int) {
			if seen == 1 || seen%100 == 0 {
				emitProgress("walking", path, seen, 0, 0)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	rep.Scanned = len(dirs)
	emitProgress("importing", "", rep.Scanned, 0, len(dirs))
	log.Info("scan complete", "category", "scan", "dirs", len(dirs))

	// Ordered processing so parent-dir NFOs are visited before child directories.
	keys := make([]string, 0, len(dirs))
	for k := range dirs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Series cache: for a detected tvshow.nfo at some parent dir, later child
	// scans can join against it.
	type seriesCtx struct {
		videoListID int64
		showTitle   string
		rootPath    string
	}
	var activeSeries *seriesCtx

	for idx, dirPath := range keys {
		bucket := dirs[dirPath]
		emitProgress("importing", dirPath, rep.Scanned, idx+1, len(keys))
		if err := ctx.Err(); err != nil {
			rep.FinishedAt = time.Now()
			rep.Duration = rep.FinishedAt.Sub(rep.StartedAt).Milliseconds()
			emitProgress("canceled", dirPath, rep.Scanned, idx+1, len(keys))
			return rep, err
		}

		// Detect a tvshow.nfo in this dir; takes precedence over per-file NFOs.
		showNFO := findNFOByRoot(bucket.NFOs, "tvshow")
		if showNFO != nil {
			series, err := i.upsertSeries(ctx, opts, bucket, showNFO)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", dirPath, err))
				log.Warn("upsert series failed", "category", "scan", "dir", dirPath, "err", err)
				continue
			}
			activeSeries = &seriesCtx{
				videoListID: series.id,
				showTitle:   series.title,
				rootPath:    dirPath,
			}
			rep.Series++
			rep.TouchedVideoListIDs = appendUnique(rep.TouchedVideoListIDs, series.id)
			// Fallthrough: also process any episode files in this same dir.
		} else if activeSeries != nil && !pathWithin(dirPath, activeSeries.rootPath) {
			// We walked past the active series root — drop it.
			activeSeries = nil
		}

		// Nothing playable here? Nothing to do.
		if len(bucket.Media) == 0 {
			continue
		}

		// Decide per-file whether this is a movie or an episode.
		for _, mediaPath := range bucket.Media {
			classified, err := i.classifyMedia(mediaPath, bucket, opts.DefaultType)
			if err != nil {
				rep.Skipped++
				log.Debug("classify skipped", "category", "scan", "path", mediaPath, "err", err)
				continue
			}
			switch classified.kind {
			case itemKindMovie:
				movie, err := i.upsertMovie(ctx, opts, bucket, mediaPath, classified)
				if err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", mediaPath, err))
					log.Warn("upsert movie failed", "category", "scan", "path", mediaPath, "err", err)
					continue
				}
				rep.Movies++
				rep.MediaRows++
				if movie != nil && movie.id > 0 {
					rep.TouchedVideoListIDs = appendUnique(rep.TouchedVideoListIDs, movie.id)
				}
			case itemKindEpisode:
				if activeSeries == nil {
					// Promote: create a minimal series from parsed folder/filename.
					series, err := i.upsertSeriesFromGuess(ctx, opts, bucket, classified)
					if err != nil {
						rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", mediaPath, err))
						log.Warn("synth series failed", "category", "scan", "path", mediaPath, "err", err)
						continue
					}
					activeSeries = &seriesCtx{
						videoListID: series.id,
						showTitle:   series.title,
						rootPath:    seriesRootFor(dirPath, classified.parsed.Season),
					}
					rep.Series++
					rep.TouchedVideoListIDs = appendUnique(rep.TouchedVideoListIDs, series.id)
				}
				sawNewSeason, err := i.upsertEpisode(ctx, opts, bucket, mediaPath,
					classified, activeSeries.videoListID, activeSeries.showTitle)
				if err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", mediaPath, err))
					log.Warn("upsert episode failed", "category", "scan", "path", mediaPath, "err", err)
					continue
				}
				if sawNewSeason {
					rep.Seasons++
				}
				rep.Episodes++
				rep.MediaRows++
				rep.TouchedVideoListIDs = appendUnique(rep.TouchedVideoListIDs, activeSeries.videoListID)
			}
		}
	}

	rep.FinishedAt = time.Now()
	rep.Duration = rep.FinishedAt.Sub(rep.StartedAt).Milliseconds()
	emitProgress("done", "", rep.Scanned, len(keys), len(keys))
	log.Info("import finished",
		"category", "scan",
		"dirs", rep.Scanned, "movies", rep.Movies,
		"series", rep.Series, "seasons", rep.Seasons, "episodes", rep.Episodes,
		"errors", len(rep.Errors))
	return rep, nil
}

// ---------------- classification ----------------

type itemKind int

const (
	itemKindMovie itemKind = iota + 1
	itemKindEpisode
)

// classifiedMedia bundles parsing results for a single media file.
type classifiedMedia struct {
	kind   itemKind
	nfo    *NFO // may be nil
	parsed ParsedName
}

// classifyMedia looks at a media file plus its directory context to decide
// whether it's a movie or an episode.
func (i *Importer) classifyMedia(mediaPath string, bucket *DirFiles, defaultType string) (*classifiedMedia, error) {
	c := &classifiedMedia{
		parsed: ParseFilename(filepath.Base(mediaPath)),
	}

	// Also look at the parent folder name for provider tags / year. When the
	// filename itself has no hint, inherit whatever the parent dir declares
	// (e.g. "A-安彦良和・板野一郎原画摄影集-2014-[tmdb=502419]/foo.mp4").
	parentHint := ParseFilename(filepath.Base(filepath.Dir(mediaPath)))
	seriesHint := seriesHintForMedia(mediaPath)
	inheritProviderHints(&c.parsed, parentHint)
	inheritProviderHints(&c.parsed, seriesHint)
	if c.parsed.Year == 0 && parentHint.Year > 0 {
		c.parsed.Year = parentHint.Year
	}
	if c.parsed.Year == 0 && seriesHint.Year > 0 {
		c.parsed.Year = seriesHint.Year
	}
	// Title: prefer the series folder for episodes, otherwise parent folder for
	// placeholder media names like "01.strm".
	if c.parsed.IsEpisode() || c.parsed.Season > 0 || ParseSeasonFolder(filepath.Base(filepath.Dir(mediaPath))) >= 0 {
		if seriesHint.Title != "" {
			c.parsed.Title = seriesHint.Title
		}
	} else if parentHint.Title != "" && looksLikePlaceholder(c.parsed.Title) {
		c.parsed.Title = parentHint.Title
	}

	// Per-file NFO (e.g. foo.mkv.nfo or foo.nfo next to foo.mkv).
	c.nfo = findNFOForMedia(mediaPath, bucket.NFOs)

	// Season folder detection: walk up one level.
	seasonFromFolder := ParseSeasonFolder(filepath.Base(filepath.Dir(mediaPath)))

	strongEpisode := c.parsed.IsEpisode() && !c.parsed.WeakEpisode
	hasTVContext := strongEpisode || c.parsed.Season > 0 || seasonFromFolder >= 0 || strings.EqualFold(defaultType, "tv")

	// Episode signals: SxxExx in filename, explicit E01/第01集, season folder, or
	// weak numeric names only when an explicit TV context exists.
	if hasTVContext {
		c.kind = itemKindEpisode
		if c.parsed.Season == 0 && seasonFromFolder >= 0 {
			c.parsed.Season = seasonFromFolder
		}
		// Episode number must be present; numeric-only filenames count.
		if c.parsed.Episode == 0 {
			return nil, fmt.Errorf("episode number missing")
		}
		return c, nil
	}

	// NFO explicitly labels the kind?
	if c.nfo != nil {
		switch c.nfo.Kind() {
		case NFOKindMovie:
			c.kind = itemKindMovie
			return c, nil
		case NFOKindEpisode:
			c.kind = itemKindEpisode
			if c.nfo.Season > 0 {
				c.parsed.Season = c.nfo.Season
			}
			if c.nfo.Episode > 0 {
				c.parsed.Episode = c.nfo.Episode
			}
			return c, nil
		}
	}

	// Fall back to DefaultType, defaulting to movie.
	if strings.EqualFold(defaultType, "tv") {
		c.kind = itemKindEpisode
		if c.parsed.Episode == 0 {
			return nil, fmt.Errorf("tv mode but no episode number in %q", filepath.Base(mediaPath))
		}
	} else {
		c.kind = itemKindMovie
	}
	return c, nil
}

func inheritProviderHints(dst *ParsedName, src ParsedName) {
	if dst.TMDBID == "" && src.TMDBID != "" {
		dst.TMDBID = src.TMDBID
	}
	if dst.IMDBID == "" && src.IMDBID != "" {
		dst.IMDBID = src.IMDBID
	}
	if dst.TVDBID == "" && src.TVDBID != "" {
		dst.TVDBID = src.TVDBID
	}
}

func seriesHintForMedia(mediaPath string) ParsedName {
	dir := filepath.Dir(mediaPath)
	if ParseSeasonFolder(filepath.Base(dir)) >= 0 {
		return ParseFilename(filepath.Base(filepath.Dir(dir)))
	}
	return ParseFilename(filepath.Base(dir))
}

// seriesRootFor yields the "root of this series" for tracking purposes.
// For a file under "/lib/Show/Season 1/ep.mkv" we want "/lib/Show".
// For "/lib/Show/ep.mkv" we want "/lib/Show".
func seriesRootFor(dir string, _ int) string {
	if ParseSeasonFolder(filepath.Base(dir)) >= 0 {
		return filepath.Dir(dir)
	}
	return dir
}

func pathWithin(path, root string) bool {
	if strings.TrimSpace(root) == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ---------------- DB upserts ----------------

type upsertResult struct {
	id    int64
	title string
}

func (i *Importer) upsertSeries(ctx context.Context, opts Options, bucket *DirFiles, nfo *NFO) (*upsertResult, error) {
	title := nfo.Title
	if title == "" {
		title = bucket.Dir
	}
	origin := nfo.OriginalTitle
	tmdbID := sql.NullString{Valid: nfo.Tmdb() != "", String: nfo.Tmdb()}
	desc := sql.NullString{Valid: nfo.Description() != "", String: nfo.Description()}
	air := sql.NullTime{Valid: !nfo.AirDate().IsZero(), Time: nfo.AirDate()}

	id, err := i.upsertVideoList(ctx, opts, db.VideoTypeTV, tmdbID, title, origin, desc, air, nfo.Runtime)
	if err != nil {
		return nil, err
	}
	i.attachImagesForList(ctx, opts, id, bucket, nfo)
	return &upsertResult{id: id, title: title}, nil
}

func (i *Importer) upsertSeriesFromGuess(ctx context.Context, opts Options, bucket *DirFiles, c *classifiedMedia) (*upsertResult, error) {
	// Try to find the series title from: season folder's parent > bucket.Dir > parsed.Title
	seasonFolderName := filepath.Base(bucket.Path)
	title := ""
	if ParseSeasonFolder(seasonFolderName) >= 0 {
		title = filepath.Base(filepath.Dir(bucket.Path))
	} else {
		title = bucket.Dir
	}
	if title == "" {
		title = c.parsed.Title
	}
	parsed := ParsedName{Title: title}
	air := sql.NullTime{}
	if y := extractYear(title); y > 0 {
		parsed.Year = y
		air = sql.NullTime{Valid: true, Time: time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	id, err := i.upsertVideoList(ctx, opts, db.VideoTypeTV,
		sql.NullString{}, parsed.Title, "", sql.NullString{}, air, 0)
	if err != nil {
		return nil, err
	}
	return &upsertResult{id: id, title: parsed.Title}, nil
}

func (i *Importer) upsertMovie(ctx context.Context, opts Options, bucket *DirFiles, mediaPath string, c *classifiedMedia) (*upsertResult, error) {
	title := c.parsed.Title
	origin := ""
	var desc sql.NullString
	var tmdbID sql.NullString
	air := sql.NullTime{}
	runtime := 0

	// TMDB id from the folder/filename tag (e.g. "[tmdb=502419]").
	if c.parsed.TMDBID != "" {
		tmdbID = sql.NullString{Valid: true, String: c.parsed.TMDBID}
	}

	if c.nfo != nil {
		if c.nfo.Title != "" {
			title = c.nfo.Title
		}
		origin = c.nfo.OriginalTitle
		if d := c.nfo.Description(); d != "" {
			desc = sql.NullString{Valid: true, String: d}
		}
		// NFO id beats filename tag when both are present.
		if t := c.nfo.Tmdb(); t != "" {
			tmdbID = sql.NullString{Valid: true, String: t}
		}
		if a := c.nfo.AirDate(); !a.IsZero() {
			air = sql.NullTime{Valid: true, Time: a}
		}
		runtime = c.nfo.Runtime
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	}

	listID, err := i.upsertVideoList(ctx, opts, db.VideoTypeMovie, tmdbID, title, origin, desc, air, runtime)
	if err != nil {
		return nil, err
	}
	if c.nfo != nil {
		i.attachImagesForList(ctx, opts, listID, bucket, c.nfo)
	} else {
		i.attachImagesForList(ctx, opts, listID, bucket, nil)
	}
	if err := i.upsertMedia(ctx, opts, listID, 0, 0, mediaPath, bucket); err != nil {
		return nil, err
	}
	return &upsertResult{id: listID, title: title}, nil
}

// upsertEpisode creates/updates season + episode + media rows. Returns true if
// a new season row was inserted for this call.
func (i *Importer) upsertEpisode(ctx context.Context, opts Options, bucket *DirFiles, mediaPath string, c *classifiedMedia, seriesListID int64, seriesTitle string) (bool, error) {
	season := c.parsed.Season
	if season == 0 {
		season = 1 // safe default if we've already committed to "this is an episode"
	}

	// Season upsert.
	var seasonID int64
	seasonTitle := fmt.Sprintf("第 %d 季", season)
	if c.nfo != nil && c.nfo.Kind() == NFOKindSeason && c.nfo.Title != "" {
		seasonTitle = c.nfo.Title
	}

	seasonIsNew := false
	err := i.db.QueryRowContext(ctx,
		"SELECT id FROM video_season WHERE video_list_id = ? AND season_number = ? LIMIT 1",
		seriesListID, season).Scan(&seasonID)
	if errors.Is(err, sql.ErrNoRows) {
		res, insErr := i.db.ExecContext(ctx, `
			INSERT INTO video_season (video_list_id, season_number, title)
			VALUES (?, ?, ?)
		`, seriesListID, season, seasonTitle)
		if insErr != nil {
			return false, fmt.Errorf("season insert: %w", insErr)
		}
		seasonID, _ = res.LastInsertId()
		seasonIsNew = true
	} else if err != nil {
		return false, err
	}

	// Episode upsert.
	epTitle := c.parsed.Title
	if epTitle == seriesTitle || looksLikePlaceholder(epTitle) {
		epTitle = fmt.Sprintf("%s E%02d", seriesTitle, c.parsed.Episode)
	}
	var desc sql.NullString
	air := sql.NullTime{}
	runtime := 0
	if c.nfo != nil {
		if c.nfo.Title != "" {
			epTitle = c.nfo.Title
		}
		if d := c.nfo.Description(); d != "" {
			desc = sql.NullString{Valid: true, String: d}
		}
		if a := c.nfo.AirDate(); !a.IsZero() {
			air = sql.NullTime{Valid: true, Time: a}
		}
		runtime = c.nfo.Runtime
	}
	if epTitle == "" {
		epTitle = fmt.Sprintf("E%02d", c.parsed.Episode)
	}

	var episodeID int64
	err = i.db.QueryRowContext(ctx,
		"SELECT id FROM video_episode WHERE video_list_id = ? AND video_season_id = ? AND episode_number = ? LIMIT 1",
		seriesListID, seasonID, c.parsed.Episode).Scan(&episodeID)
	if errors.Is(err, sql.ErrNoRows) {
		res, insErr := i.db.ExecContext(ctx, `
			INSERT INTO video_episode (video_list_id, video_season_id, episode_number,
				title, description, date_air, runtime)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, seriesListID, seasonID, c.parsed.Episode, epTitle, desc, air, nullableInt(runtime))
		if insErr != nil {
			return seasonIsNew, fmt.Errorf("episode insert: %w", insErr)
		}
		episodeID, _ = res.LastInsertId()

		// Fan-out subscription events for all users following this series.
		_, fanErr := i.db.ExecContext(ctx, `
			INSERT INTO series_subscription_event (user_id, video_list_id, video_episode_id)
			SELECT user_id, ?, ? FROM series_subscription WHERE video_list_id = ?
			ON CONFLICT DO NOTHING
		`, seriesListID, episodeID, seriesListID)
		if fanErr != nil && i.log != nil {
			i.log.Warn("subscription event fan-out failed", "series", seriesListID, "episode", episodeID, "err", fanErr)
		}
	} else if err != nil {
		return seasonIsNew, err
	} else {
		_, _ = i.db.ExecContext(ctx, `
			UPDATE video_episode
			SET title = ?, description = ?, date_air = ?, runtime = ?, deleted_at = NULL
			WHERE id = ?
		`, epTitle, desc, air, nullableInt(runtime), episodeID)
	}

	// Attach images to the episode (thumb for episodes is conventional).
	if c.nfo != nil {
		if poster := c.nfo.PosterURL(); poster != "" {
			_ = i.attachImage(ctx, opts, "Primary", "ve", episodeID, urlKindFromString(poster), poster)
		}
	}
	// Media row.
	if err := i.upsertMedia(ctx, opts, seriesListID, seasonID, episodeID, mediaPath, bucket); err != nil {
		return seasonIsNew, err
	}
	return seasonIsNew, nil
}

// upsertVideoList inserts or updates the video_list row keyed by (video_type, tmdb_id) when
// tmdb_id is set, otherwise by (video_library_id, video_type, title).
func (i *Importer) upsertVideoList(
	ctx context.Context, opts Options, videoType string,
	tmdbID sql.NullString, title, originTitle string,
	description sql.NullString, dateAir sql.NullTime, runtime int,
) (int64, error) {
	if opts.DryRun {
		return 0, nil
	}

	origin := sql.NullString{Valid: originTitle != "", String: originTitle}
	runtimeVal := nullableInt(runtime)

	var existingID int64
	var err error
	if tmdbID.Valid && tmdbID.String != "" {
		err = i.db.QueryRowContext(ctx,
			"SELECT id FROM video_list WHERE video_type = ? AND tmdb_id = ? LIMIT 1",
			videoType, tmdbID.String).Scan(&existingID)
	} else {
		err = i.db.QueryRowContext(ctx,
			"SELECT id FROM video_list WHERE video_library_id = ? AND video_type = ? AND title = ? AND tmdb_id IS NULL LIMIT 1",
			opts.LibraryID, videoType, title).Scan(&existingID)
	}

	if errors.Is(err, sql.ErrNoRows) {
		res, insErr := i.db.ExecContext(ctx, `
			INSERT INTO video_list
				(video_library_id, video_type, tmdb_id, title, origin_title,
				 description, date_air, runtime)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, opts.LibraryID, videoType, tmdbID, title, origin, description, dateAir, runtimeVal)
		if insErr != nil {
			return 0, fmt.Errorf("video_list insert: %w", insErr)
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}

	_, err = i.db.ExecContext(ctx, `
		UPDATE video_list
		SET title = ?, origin_title = ?, description = ?, date_air = ?, runtime = ?, deleted_at = NULL
		WHERE id = ?
	`, title, origin, description, dateAir, runtimeVal, existingID)
	return existingID, err
}

// upsertMedia inserts (or refreshes) a video_media row.
// videoSeasonID and videoEpisodeID may be 0 for movies.
func (i *Importer) upsertMedia(
	ctx context.Context, opts Options,
	videoListID int64, videoSeasonID, videoEpisodeID int64,
	mediaPath string, bucket *DirFiles,
) error {
	if opts.DryRun {
		return nil
	}

	pathType, pathURL, err := resolveMediaPath(mediaPath)
	if err != nil {
		return err
	}

	// File size from disk for local or strm->local (best-effort).
	var fileSize int64
	var fileSecond int64
	var fileMetadata []byte
	if pathType == db.PathTypeLocal {
		if info, err := os.Stat(pathURL); err == nil {
			fileSize = info.Size()
		}
	}

	name := filepath.Base(mediaPath)
	container := strings.TrimPrefix(strings.ToLower(filepath.Ext(mediaPath)), ".")
	if opts.ProbeMedia {
		if probe, err := ProbeLocalMedia(ctx, pathURL); err == nil {
			fileMetadata = probe.Metadata
			fileSecond = probe.Duration
			if fileSize == 0 && probe.Size > 0 {
				fileSize = probe.Size
			}
			if probe.Container != "" {
				container = probe.Container
			}
		} else if i.log != nil {
			i.log.Debug("ffprobe media info unavailable", "category", "scan", "path", pathURL, "err", err)
		}
	}

	// Look up existing media by path to make re-scans idempotent.
	var existingID int64
	err = i.db.QueryRowContext(ctx, `
		SELECT id FROM video_media
		WHERE video_list_id = ?
		  AND video_episode_id IS NOT DISTINCT FROM ?
		  AND (path_url = ? OR name = ?)
		LIMIT 1
	`, videoListID, nullableInt64(videoEpisodeID), pathURL, name).Scan(&existingID)

	nullSeason := nullableInt64(videoSeasonID)
	nullEp := nullableInt64(videoEpisodeID)
	fileSizeN := nullableInt64(fileSize)
	fileSecondN := nullableInt64(fileSecond)
	fileMetadataN := sql.NullString{Valid: len(fileMetadata) > 0, String: string(fileMetadata)}

	mediaID := existingID
	if errors.Is(err, sql.ErrNoRows) {
		uuid := newUUID()
		res, insErr := i.db.ExecContext(ctx, `
			INSERT INTO video_media
				(uuid, video_list_id, video_season_id, video_episode_id,
				 name, status, file_size, file_second, file_matadata, file_container, path_type, path_url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, uuid, videoListID, nullSeason, nullEp,
			name, db.MediaStatusComplete, fileSizeN, fileSecondN, fileMetadataN, container, pathType, pathURL)
		if insErr != nil {
			return fmt.Errorf("media insert: %w", insErr)
		}
		mediaID, _ = res.LastInsertId()
	} else if err != nil {
		return err
	} else {
		_, err := i.db.ExecContext(ctx, `
			UPDATE video_media
			SET name = ?, status = ?, file_size = ?, file_second = ?, file_matadata = ?, file_container = ?,
			    path_type = ?, path_url = ?, deleted_at = NULL
			WHERE id = ?
		`, name, db.MediaStatusComplete, fileSizeN, fileSecondN, fileMetadataN, container, pathType, pathURL, existingID)
		if err != nil {
			return err
		}
	}

	// Attach sidecar subtitles: file with same base name, different extension.
	baseNoExt := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	for _, sub := range bucket.Subtitles {
		subBase := filepath.Base(sub)
		if !strings.HasPrefix(subBase, baseNoExt) {
			continue
		}
		if mediaID == 0 {
			continue
		}
		codec := strings.TrimPrefix(strings.ToLower(filepath.Ext(subBase)), ".")
		title := subBase
		// Skip if already attached.
		var existing int64
		_ = i.db.QueryRowContext(ctx,
			"SELECT id FROM video_subtitle WHERE video_media_id = ? AND path_url = ? LIMIT 1",
			mediaID, sub).Scan(&existing)
		if existing > 0 {
			continue
		}
		_, _ = i.db.ExecContext(ctx, `
			INSERT INTO video_subtitle (video_media_id, title, codec, path_type, path_url)
			VALUES (?, ?, ?, ?, ?)
		`, mediaID, title, codec, db.PathTypeLocal, sub)
	}
	return nil
}

// attachImagesForList adds Primary/Backdrop/Thumb rows for a video_list.
// Prefers image URLs declared in NFO <art>/<thumb>, falls back to sidecar files
// (poster.jpg, fanart.jpg, folder.jpg).
func (i *Importer) attachImagesForList(ctx context.Context, opts Options, listID int64, bucket *DirFiles, nfo *NFO) {
	// Primary
	if nfo != nil {
		if p := nfo.PosterURL(); p != "" {
			_ = i.attachImage(ctx, opts, "Primary", "vl", listID, urlKindFromString(p), p)
		}
		if b := nfo.BackdropURL(); b != "" {
			_ = i.attachImage(ctx, opts, "Backdrop", "vl", listID, urlKindFromString(b), b)
		}
	}
	// Sidecar files (used when NFO lacks them).
	for _, img := range bucket.Images {
		base := strings.ToLower(filepath.Base(img))
		switch {
		case strings.HasPrefix(base, "poster") || strings.HasPrefix(base, "folder"):
			_ = i.attachImage(ctx, opts, "Primary", "vl", listID, db.PathTypeLocal, img)
		case strings.HasPrefix(base, "fanart") || strings.HasPrefix(base, "backdrop"):
			_ = i.attachImage(ctx, opts, "Backdrop", "vl", listID, db.PathTypeLocal, img)
		case strings.HasPrefix(base, "logo"):
			_ = i.attachImage(ctx, opts, "Logo", "vl", listID, db.PathTypeLocal, img)
		case strings.HasPrefix(base, "thumb"):
			_ = i.attachImage(ctx, opts, "Thumb", "vl", listID, db.PathTypeLocal, img)
		}
	}
}

// attachImage inserts a video_image row unless one with the same (type, relation, type) already exists.
// path_type respects the ImagePathType constants (tmdb/douban/url/local).
func (i *Importer) attachImage(ctx context.Context, opts Options, imgType, relType string, relID int64, pathType, pathURL string) error {
	if opts.DryRun {
		return nil
	}
	if pathURL == "" {
		return nil
	}
	var existing int64
	_ = i.db.QueryRowContext(ctx,
		"SELECT id FROM video_image WHERE relation_type = ? AND relation_id = ? AND type = ? AND deleted_at IS NULL LIMIT 1",
		relType, relID, imgType,
	).Scan(&existing)
	if existing > 0 {
		// Update URL / type in case it changed on rescan.
		_, err := i.db.ExecContext(ctx,
			"UPDATE video_image SET path_type = ?, path_url = ? WHERE id = ?",
			pathType, pathURL, existing)
		return err
	}
	_, err := i.db.ExecContext(ctx, `
		INSERT INTO video_image (type, relation_type, relation_id, path_type, path_url)
		VALUES (?, ?, ?, ?, ?)
	`, imgType, relType, relID, pathType, pathURL)
	return err
}

// ---------------- helpers ----------------

// resolveMediaPath returns (path_type, path_url, error) for a scanned file.
// .strm is resolved to its first target (preserving URL vs local distinction).
func resolveMediaPath(mediaPath string) (pathType, pathURL string, err error) {
	if classifyExt(mediaPath) == FileKindSTRM {
		s, err := ParseSTRM(mediaPath)
		if err != nil {
			return "", "", fmt.Errorf("parse strm %s: %w", mediaPath, err)
		}
		if s.Primary == "" {
			return "", "", fmt.Errorf("empty strm %s", mediaPath)
		}
		if s.IsURL {
			return db.PathTypeURL, s.Primary, nil
		}
		return db.PathTypeLocal, s.Primary, nil
	}
	return db.PathTypeLocal, mediaPath, nil
}

// urlKindFromString picks an image path_type based on the URL shape.
func urlKindFromString(s string) string {
	switch {
	case strings.HasPrefix(s, "/") && !strings.Contains(s, "://"):
		// Looks like a TMDB-style relative path "/aBc.jpg".
		if strings.Count(s, "/") == 1 {
			return db.ImagePathTypeTMDB
		}
		return db.PathTypeLocal
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		if strings.Contains(s, "douban") {
			return db.ImagePathTypeDouban
		}
		return db.ImagePathTypeURL
	}
	return db.PathTypeLocal
}

// findNFOByRoot returns the NFO whose XML root name matches (case-insensitive),
// or nil if none is present. Useful for finding tvshow.nfo among many NFOs.
func findNFOByRoot(paths []string, root string) *NFO {
	for _, p := range paths {
		if !strings.EqualFold(filepath.Base(p), root+".nfo") {
			continue
		}
		nfo, err := ParseNFO(p)
		if err != nil {
			continue
		}
		if strings.EqualFold(string(nfo.Kind()), root) ||
			strings.EqualFold(nfo.XMLName.Local, root) {
			return nfo
		}
	}
	// Fallback: any NFO whose root element matches, regardless of filename.
	for _, p := range paths {
		nfo, err := ParseNFO(p)
		if err != nil {
			continue
		}
		if strings.EqualFold(nfo.XMLName.Local, root) {
			return nfo
		}
	}
	return nil
}

// findNFOForMedia returns the NFO sitting next to the media file, if any.
// Common layouts:
//
//	foo.mkv + foo.nfo
//	foo.mkv + foo.mkv.nfo
//	foo.strm + foo.nfo
//	(and a single movie.nfo in the dir when there's only one video)
func findNFOForMedia(mediaPath string, nfos []string) *NFO {
	baseNoExt := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	for _, n := range nfos {
		nbase := filepath.Base(n)
		// foo.nfo, foo.mkv.nfo
		if nbase == baseNoExt+".nfo" ||
			strings.EqualFold(nbase, filepath.Base(mediaPath)+".nfo") {
			if parsed, err := ParseNFO(n); err == nil {
				return parsed
			}
		}
	}
	// Single movie.nfo in the directory.
	for _, n := range nfos {
		if strings.EqualFold(filepath.Base(n), "movie.nfo") {
			if parsed, err := ParseNFO(n); err == nil {
				return parsed
			}
		}
	}
	return nil
}

// nullableInt returns sql.NullInt64 where 0 ⇒ NULL.
func nullableInt(v int) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: int64(v)}
}

// nullableInt64 returns sql.NullInt64 where 0 ⇒ NULL.
func nullableInt64(v int64) sql.NullInt64 {
	if v <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Valid: true, Int64: v}
}

// newUUID generates an RFC4122 v4 UUID without external deps.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	// xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	hex.EncodeToString(b[:])
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Render returns a JSON summary for the admin endpoint.
func (r *Report) Render() ([]byte, error) { return json.Marshal(r) }

// appendUnique appends id to dst if not already present.
func appendUnique(dst []int64, id int64) []int64 {
	for _, x := range dst {
		if x == id {
			return dst
		}
	}
	return append(dst, id)
}
