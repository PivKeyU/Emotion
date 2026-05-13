package db

import (
	"database/sql"
	"time"
)

// NullString, NullInt etc. are just aliases so call-sites read cleanly.
type (
	NullString = sql.NullString
	NullInt64  = sql.NullInt64
	NullBool   = sql.NullBool
	NullTime   = sql.NullTime
)

// Library mirrors the `library` table.
type Library struct {
	ID        int64
	Name      string
	Role      NullString
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt NullTime
}

// User mirrors the `user` table.
type User struct {
	ID         int64
	Username   NullString
	Password   NullString
	Folders    NullString // raw JSON
	IsCanDown  NullBool
	IsAdmin    NullBool
	IsDisable  NullBool
	Remark     NullString
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  NullTime
}

// Token mirrors the `token` table.
type Token struct {
	ID            int64
	Token         string
	UserID        int64
	DeviceClient  NullString
	DeviceName    NullString
	DeviceID      NullString
	DeviceVersion NullString
	CreatedAt     time.Time
	LastUsedAt    NullTime
}

// Favorite mirrors the `favorites` table row.
type Favorite struct {
	ID           int64
	RelationType string
	RelationID   int64
	UserID       int64
	CreatedAt    time.Time
}

// UserVideoRecord mirrors the `user_video_record` table.
type UserVideoRecord struct {
	ID             int64
	VideoListID    int64
	VideoSeasonID  NullInt64
	VideoEpisodeID NullInt64
	VideoMediaID   NullInt64
	PlaySeconds    NullInt64
	IsComplete     NullBool
	UserID         int64
	UpdatedAt      time.Time
}

// VideoList mirrors the `video_list` table.
type VideoList struct {
	ID             int64
	VideoLibraryID int64
	VideoType      string
	TmdbID         NullString
	Title          string
	OriginTitle    NullString
	Description    NullString
	Tagline        NullString
	Genres         NullString // raw JSON
	Peoples        NullString // raw JSON
	Upcoming       NullString
	DateAir        NullTime
	Runtime        NullInt64
	Remark         NullString
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      NullTime
}

// VideoSeason mirrors the `video_season` table.
type VideoSeason struct {
	ID                 int64
	VideoListID        int64
	SeasonNumber       int64
	SeasonNumberCustom NullInt64
	Title              string
	Description        NullString
	DateAir            NullTime
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          NullTime
}

// VideoEpisode mirrors the `video_episode` table.
type VideoEpisode struct {
	ID            int64
	VideoListID   int64
	VideoSeasonID int64
	EpisodeNumber int64
	Title         string
	Description   NullString
	DateAir       NullTime
	Runtime       NullInt64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     NullTime
}

// VideoMedia mirrors the `video_media` table.
type VideoMedia struct {
	ID             int64
	UUID           string
	VideoListID    int64
	VideoSeasonID  NullInt64
	VideoEpisodeID NullInt64
	Name           string
	Status         string
	FileSize       NullInt64
	FileSecond     NullInt64
	FileMetadata   NullString // raw JSON
	FileContainer  NullString
	FileChapters   NullString // raw JSON
	PathType       NullString
	PathURL        NullString
	UserID         NullInt64
	NumberView     int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      NullTime
}

// VideoSubtitle mirrors the `video_subtitle` table.
type VideoSubtitle struct {
	ID           int64
	VideoMediaID int64
	Title        string
	Codec        string
	PathType     NullString
	PathURL      NullString
	UserID       NullInt64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    NullTime
}

// VideoImage mirrors the `video_image` table.
type VideoImage struct {
	ID           int64
	Type         string
	RelationType string
	RelationID   int64
	PathType     NullString
	PathURL      NullString
	UserID       NullInt64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    NullTime
}
