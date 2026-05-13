package db

// VideoType constants for video_list.video_type.
const (
	VideoTypeTV    = "tv"
	VideoTypeMovie = "movie"
)

// VideoMediaStatus values for video_media.status.
const (
	MediaStatusDefault  = "default"
	MediaStatusReview   = "review"
	MediaStatusRefuse   = "refuse"
	MediaStatusComplete = "complete"
)

// VideoMediaPathType values for video_media.path_type and video_subtitle.path_type.
const (
	PathTypeLocal = "local"
	PathTypeURL   = "url"
)

// VideoImagePathType values for video_image.path_type.
const (
	ImagePathTypeTMDB   = "tmdb"
	ImagePathTypeDouban = "douban"
	ImagePathTypeURL    = "url"
)

// VideoImageType values for video_image.type.
const (
	ImageTypeLogo     = "Logo"
	ImageTypePrimary  = "Primary"
	ImageTypeBackdrop = "Backdrop"
	ImageTypeThumb    = "Thumb"
)

// Library role values.
const (
	LibraryRolePublic = "public"
	LibraryRoleHidden = "hidden"
)
