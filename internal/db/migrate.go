package db

import "fmt"

// migrations is the ordered set of PostgreSQL DDL statements applied at startup.
// Each statement is idempotent so running repeatedly is safe.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS library (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		name VARCHAR(255) NOT NULL,
		role VARCHAR(255) NULL,
		root_path TEXT NULL,
		watch_enabled BOOLEAN NOT NULL DEFAULT false,
		watch_interval_seconds INTEGER NOT NULL DEFAULT 30
	)`,
	`ALTER TABLE library ADD COLUMN IF NOT EXISTS root_path TEXT NULL`,
	`ALTER TABLE library ADD COLUMN IF NOT EXISTS watch_enabled BOOLEAN NOT NULL DEFAULT false`,
	`ALTER TABLE library ADD COLUMN IF NOT EXISTS watch_interval_seconds INTEGER NOT NULL DEFAULT 30`,
	`CREATE INDEX IF NOT EXISTS idx_library_name ON library (name)`,
	`CREATE INDEX IF NOT EXISTS idx_library_deleted ON library (deleted_at)`,

	`CREATE TABLE IF NOT EXISTS admin_session (
		id BIGSERIAL PRIMARY KEY,
		token VARCHAR(64) NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_used_at TIMESTAMPTZ NULL,
		expires_at TIMESTAMPTZ NULL,
		revoked_at TIMESTAMPTZ NULL,
		CONSTRAINT unx_admin_session_token UNIQUE (token)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_session_token ON admin_session (token)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_session_revoked ON admin_session (revoked_at)`,

	`CREATE TABLE IF NOT EXISTS admin_api_key (
		id BIGSERIAL PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		remark TEXT NULL,
		token_hash CHAR(64) NOT NULL,
		token_prefix VARCHAR(16) NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_used_at TIMESTAMPTZ NULL,
		revoked_at TIMESTAMPTZ NULL,
		CONSTRAINT unx_admin_api_key_hash UNIQUE (token_hash)
	)`,
	`ALTER TABLE admin_api_key ADD COLUMN IF NOT EXISTS remark TEXT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_admin_api_key_hash ON admin_api_key (token_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_api_key_revoked ON admin_api_key (revoked_at)`,

	`CREATE TABLE IF NOT EXISTS app_setting (
		key VARCHAR(128) PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,

	`CREATE TABLE IF NOT EXISTS app_user (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		username VARCHAR(255) NULL,
		password VARCHAR(255) NULL,
		folders JSONB NULL,
		is_can_down BOOLEAN NULL,
		is_admin BOOLEAN NULL,
		is_disable BOOLEAN NULL,
		remark VARCHAR(255) NULL,
		CONSTRAINT unx_user UNIQUE (username)
	)`,
	`ALTER TABLE app_user DROP CONSTRAINT IF EXISTS unx_user`,
	`CREATE UNIQUE INDEX IF NOT EXISTS unx_user_active_username ON app_user (username) WHERE deleted_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS token (
		id BIGSERIAL PRIMARY KEY,
		token VARCHAR(64) NOT NULL,
		user_id BIGINT NOT NULL,
		device_client VARCHAR(255) NULL,
		device_name VARCHAR(255) NULL,
		device_id VARCHAR(255) NULL,
		device_version VARCHAR(255) NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_used_at TIMESTAMPTZ NULL,
		CONSTRAINT uni_token UNIQUE (token)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_token_user_id ON token (user_id)`,

	`CREATE TABLE IF NOT EXISTS favorites (
		id BIGSERIAL PRIMARY KEY,
		relation_type VARCHAR(255) NOT NULL,
		relation_id BIGINT NOT NULL,
		user_id BIGINT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		CONSTRAINT unx_favorites UNIQUE (relation_type, relation_id, user_id)
	)`,

	`CREATE TABLE IF NOT EXISTS user_video_record (
		id BIGSERIAL PRIMARY KEY,
		video_list_id BIGINT NOT NULL,
		video_season_id BIGINT NULL,
		video_episode_id BIGINT NULL,
		video_media_id BIGINT NULL,
		play_seconds BIGINT NULL,
		is_complete BOOLEAN NULL,
		user_id BIGINT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_list ON user_video_record (video_list_id)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_season ON user_video_record (video_season_id)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_episode ON user_video_record (video_episode_id)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_media ON user_video_record (video_media_id)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_user ON user_video_record (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_user_list_episode_updated ON user_video_record (user_id, video_list_id, video_episode_id, updated_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_user_episode_updated ON user_video_record (user_id, video_episode_id, updated_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_uvr_resume ON user_video_record (user_id, updated_at DESC) WHERE play_seconds IS NOT NULL AND COALESCE(is_complete, false) = false`,

	`CREATE TABLE IF NOT EXISTS video_list (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		video_library_id BIGINT NOT NULL,
		video_type VARCHAR(32) NOT NULL,
		tmdb_id VARCHAR(255) NULL,
		imdb_id VARCHAR(64) NULL,
		tvdb_id VARCHAR(64) NULL,
		title VARCHAR(255) NOT NULL,
		origin_title VARCHAR(255) NULL,
		description TEXT NULL,
		tagline TEXT NULL,
		genres JSONB NULL,
		peoples JSONB NULL,
		upcoming VARCHAR(255) NULL,
		date_air DATE NULL,
		runtime INTEGER NULL,
		remark VARCHAR(255) NULL,
		CONSTRAINT unx_list UNIQUE (video_type, tmdb_id)
	)`,
	`ALTER TABLE video_list ADD COLUMN IF NOT EXISTS imdb_id VARCHAR(64) NULL`,
	`ALTER TABLE video_list ADD COLUMN IF NOT EXISTS tvdb_id VARCHAR(64) NULL`,
	`CREATE INDEX IF NOT EXISTS idx_vl_library ON video_list (video_library_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_imdb ON video_list (imdb_id) WHERE imdb_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_vl_tvdb ON video_list (tvdb_id) WHERE tvdb_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS idx_vl_title ON video_list (title)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_origin_title ON video_list (origin_title)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_date_air ON video_list (date_air)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_updated ON video_list (updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_deleted ON video_list (deleted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_library_updated_active ON video_list (video_library_id, deleted_at, updated_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_library_type_updated_active ON video_list (video_library_id, video_type, deleted_at, updated_at DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_vl_scan_title ON video_list (video_library_id, video_type, title) WHERE tmdb_id IS NULL`,

	`CREATE TABLE IF NOT EXISTS video_list_title_alias (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		video_list_id BIGINT NOT NULL,
		title VARCHAR(255) NOT NULL,
		user_id BIGINT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vlta_list ON video_list_title_alias (video_list_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vlta_title ON video_list_title_alias (title)`,
	`CREATE INDEX IF NOT EXISTS idx_vlta_deleted ON video_list_title_alias (deleted_at)`,

	`CREATE TABLE IF NOT EXISTS video_season (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		video_list_id BIGINT NOT NULL,
		season_number BIGINT NOT NULL,
		season_number_custom BIGINT NULL,
		title VARCHAR(255) NOT NULL,
		description TEXT NULL,
		date_air DATE NULL,
		CONSTRAINT unx_season UNIQUE (video_list_id, season_number)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vs_list_deleted ON video_season (video_list_id, deleted_at)`,

	`CREATE TABLE IF NOT EXISTS video_episode (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		video_list_id BIGINT NOT NULL,
		video_season_id BIGINT NOT NULL,
		episode_number BIGINT NOT NULL,
		title VARCHAR(255) NOT NULL,
		description TEXT NULL,
		date_air DATE NULL,
		runtime INTEGER NULL,
		CONSTRAINT unx_episode UNIQUE (video_list_id, video_season_id, episode_number)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ve_season ON video_episode (video_season_id)`,
	`CREATE INDEX IF NOT EXISTS idx_ve_list_deleted ON video_episode (video_list_id, deleted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_ve_date_air ON video_episode (date_air)`,

	`CREATE TABLE IF NOT EXISTS video_image (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		type VARCHAR(64) NOT NULL,
		relation_type VARCHAR(64) NOT NULL,
		relation_id BIGINT NOT NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vi_rel ON video_image (relation_type, relation_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vi_rel_type ON video_image (relation_type, relation_id, type, deleted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_vi_user ON video_image (user_id)`,

	`CREATE TABLE IF NOT EXISTS video_media (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		uuid CHAR(36) NOT NULL,
		video_list_id BIGINT NOT NULL,
		video_season_id BIGINT NULL,
		video_episode_id BIGINT NULL,
		name VARCHAR(255) NOT NULL,
		status VARCHAR(32) NOT NULL,
		file_size BIGINT NULL,
		file_second BIGINT NULL,
		file_matadata JSONB NULL,
		file_container VARCHAR(64) NULL,
		file_chapters JSONB NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT NULL,
		number_view BIGINT NOT NULL DEFAULT 0,
		CONSTRAINT unx_media_uuid UNIQUE (uuid)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_user ON video_media (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_list ON video_media (video_list_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_list_deleted ON video_media (video_list_id, deleted_at)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_season ON video_media (video_season_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_episode ON video_media (video_episode_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_path_url ON video_media (path_url)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_list_episode_name ON video_media (video_list_id, video_episode_id, name)`,
	`CREATE INDEX IF NOT EXISTS idx_vm_scan_path ON video_media (video_list_id, video_episode_id, path_url)`,

	`CREATE TABLE IF NOT EXISTS video_subtitle (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		video_media_id BIGINT NOT NULL,
		title VARCHAR(255) NOT NULL,
		codec VARCHAR(64) NOT NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_vs_media ON video_subtitle (video_media_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vs_user ON video_subtitle (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vs_media_path ON video_subtitle (video_media_id, path_url)`,

	`CREATE TABLE IF NOT EXISTS video_genre (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		tmdb_id VARCHAR(255) NOT NULL,
		name VARCHAR(255) NOT NULL,
		CONSTRAINT unx_genre UNIQUE (tmdb_id)
	)`,

	`CREATE TABLE IF NOT EXISTS video_people (
		id BIGSERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ NULL,
		tmdb_id VARCHAR(255) NOT NULL,
		type VARCHAR(64) NOT NULL,
		name VARCHAR(255) NOT NULL,
		original_name VARCHAR(255) NULL,
		gender SMALLINT NOT NULL DEFAULT 0,
		description TEXT NULL,
		birthday DATE NULL,
		deathday DATE NULL,
		CONSTRAINT unx_people UNIQUE (tmdb_id)
	)`,

	`CREATE TABLE IF NOT EXISTS playback_activity (
		id BIGSERIAL PRIMARY KEY,
		date_created TIMESTAMPTZ NOT NULL DEFAULT now(),
		user_id VARCHAR(64) NOT NULL,
		item_id VARCHAR(64) NULL,
		item_type VARCHAR(64) NULL,
		item_name VARCHAR(255) NULL,
		play_method VARCHAR(64) NULL,
		client VARCHAR(64) NULL,
		device_name VARCHAR(255) NULL,
		play_duration BIGINT NOT NULL DEFAULT 0,
		pause_duration BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_pa_user ON playback_activity (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_pa_date ON playback_activity (date_created)`,
	`ALTER TABLE playback_activity ADD COLUMN IF NOT EXISTS remote_address VARCHAR(64)`,

	`CREATE TABLE IF NOT EXISTS series_subscription (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL,
		video_list_id BIGINT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		CONSTRAINT unx_series_subscription UNIQUE (user_id, video_list_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subs_user ON series_subscription (user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_subs_series ON series_subscription (video_list_id)`,

	`CREATE TABLE IF NOT EXISTS series_subscription_event (
		id BIGSERIAL PRIMARY KEY,
		user_id BIGINT NOT NULL,
		video_list_id BIGINT NOT NULL,
		video_episode_id BIGINT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		delivered_at TIMESTAMPTZ NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_subev_pending ON series_subscription_event (delivered_at) WHERE delivered_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS idx_subev_user ON series_subscription_event (user_id)`,
}

// Migrate applies the schema. Safe to call every startup.
func Migrate(d *DB) error {
	for i, stmt := range migrations {
		if _, err := d.Exec(stmt); err != nil {
			return fmt.Errorf("migration #%d failed: %w", i, err)
		}
	}
	return nil
}
