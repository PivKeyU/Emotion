package db

import (
	"database/sql"
	"fmt"
)

// migrations is the ordered set of DDL statements applied at startup.
// Each statement is idempotent (IF NOT EXISTS) so running repeatedly is safe.
// Schema mirrors emya (src/db/schema/*).
var migrations = []string{
	// library
	`CREATE TABLE IF NOT EXISTS library (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		name VARCHAR(255) NOT NULL,
		role VARCHAR(255) NULL,
		PRIMARY KEY (id),
		KEY idx_library_name (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// user
	`CREATE TABLE IF NOT EXISTS user (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		username VARCHAR(255) NULL,
		password VARCHAR(255) NULL,
		folders JSON NULL,
		is_can_down BOOLEAN NULL,
		is_admin BOOLEAN NULL,
		is_disable BOOLEAN NULL,
		remark VARCHAR(255) NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_user (username)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// token
	`CREATE TABLE IF NOT EXISTS token (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		token VARCHAR(64) NOT NULL,
		user_id BIGINT UNSIGNED NOT NULL,
		device_client VARCHAR(255) NULL,
		device_name VARCHAR(255) NULL,
		device_id VARCHAR(255) NULL,
		device_version VARCHAR(255) NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at TIMESTAMP NULL,
		PRIMARY KEY (id),
		UNIQUE KEY uni_token (token),
		KEY idx_token_user_id (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// favorites
	`CREATE TABLE IF NOT EXISTS favorites (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		relation_type VARCHAR(255) NOT NULL,
		relation_id BIGINT UNSIGNED NOT NULL,
		user_id BIGINT UNSIGNED NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		UNIQUE KEY unx_favorites (relation_type, relation_id, user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// user_video_record
	`CREATE TABLE IF NOT EXISTS user_video_record (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		video_list_id BIGINT UNSIGNED NOT NULL,
		video_season_id BIGINT UNSIGNED NULL,
		video_episode_id BIGINT UNSIGNED NULL,
		video_media_id BIGINT UNSIGNED NULL,
		play_seconds BIGINT UNSIGNED NULL,
		is_complete BOOLEAN NULL,
		user_id BIGINT UNSIGNED NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		KEY idx_uvr_list (video_list_id),
		KEY idx_uvr_season (video_season_id),
		KEY idx_uvr_episode (video_episode_id),
		KEY idx_uvr_media (video_media_id),
		KEY idx_uvr_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_list
	`CREATE TABLE IF NOT EXISTS video_list (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		video_library_id BIGINT UNSIGNED NOT NULL,
		video_type VARCHAR(32) NOT NULL,
		tmdb_id VARCHAR(255) NULL,
		title VARCHAR(255) NOT NULL,
		origin_title VARCHAR(255) NULL,
		description TEXT NULL,
		tagline TEXT NULL,
		genres JSON NULL,
		peoples JSON NULL,
		upcoming VARCHAR(255) NULL,
		date_air DATE NULL,
		runtime SMALLINT UNSIGNED NULL,
		remark VARCHAR(255) NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_list (video_type, tmdb_id),
		KEY idx_vl_library (video_library_id),
		KEY idx_vl_title (title),
		KEY idx_vl_origin_title (origin_title),
		KEY idx_vl_date_air (date_air),
		KEY idx_vl_updated (updated_at),
		KEY idx_vl_deleted (deleted_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_list_title_alias
	`CREATE TABLE IF NOT EXISTS video_list_title_alias (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		video_list_id BIGINT UNSIGNED NOT NULL,
		title VARCHAR(255) NOT NULL,
		user_id BIGINT UNSIGNED NULL,
		PRIMARY KEY (id),
		KEY idx_vlta_list (video_list_id),
		KEY idx_vlta_title (title),
		KEY idx_vlta_deleted (deleted_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_season
	`CREATE TABLE IF NOT EXISTS video_season (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		video_list_id BIGINT UNSIGNED NOT NULL,
		season_number BIGINT UNSIGNED NOT NULL,
		season_number_custom BIGINT UNSIGNED NULL,
		title VARCHAR(255) NOT NULL,
		description TEXT NULL,
		date_air DATE NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_season (video_list_id, season_number)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_episode
	`CREATE TABLE IF NOT EXISTS video_episode (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		video_list_id BIGINT UNSIGNED NOT NULL,
		video_season_id BIGINT UNSIGNED NOT NULL,
		episode_number BIGINT UNSIGNED NOT NULL,
		title VARCHAR(255) NOT NULL,
		description TEXT NULL,
		date_air DATE NULL,
		runtime SMALLINT UNSIGNED NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_episode (video_list_id, video_season_id, episode_number),
		KEY idx_ve_season (video_season_id),
		KEY idx_ve_date_air (date_air)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_image
	`CREATE TABLE IF NOT EXISTS video_image (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		type VARCHAR(64) NOT NULL,
		relation_type VARCHAR(64) NOT NULL,
		relation_id BIGINT UNSIGNED NOT NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT UNSIGNED NULL,
		PRIMARY KEY (id),
		KEY idx_vi_rel (relation_type, relation_id),
		KEY idx_vi_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_media
	`CREATE TABLE IF NOT EXISTS video_media (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		uuid CHAR(36) NOT NULL,
		video_list_id BIGINT UNSIGNED NOT NULL,
		video_season_id BIGINT UNSIGNED NULL,
		video_episode_id BIGINT UNSIGNED NULL,
		name VARCHAR(255) NOT NULL,
		status VARCHAR(32) NOT NULL,
		file_size BIGINT UNSIGNED NULL,
		file_second BIGINT UNSIGNED NULL,
		file_matadata JSON NULL,
		file_container VARCHAR(64) NULL,
		file_chapters JSON NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT UNSIGNED NULL,
		number_view BIGINT UNSIGNED NOT NULL DEFAULT 0,
		PRIMARY KEY (id),
		UNIQUE KEY unx_media_uuid (uuid),
		KEY idx_vm_user (user_id),
		KEY idx_vm_list (video_list_id),
		KEY idx_vm_season (video_season_id),
		KEY idx_vm_episode (video_episode_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_subtitle
	`CREATE TABLE IF NOT EXISTS video_subtitle (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		video_media_id BIGINT UNSIGNED NOT NULL,
		title VARCHAR(255) NOT NULL,
		codec VARCHAR(64) NOT NULL,
		path_type VARCHAR(64) NULL,
		path_url TEXT NULL,
		user_id BIGINT UNSIGNED NULL,
		PRIMARY KEY (id),
		KEY idx_vs_media (video_media_id),
		KEY idx_vs_user (user_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_genre
	`CREATE TABLE IF NOT EXISTS video_genre (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		tmdb_id VARCHAR(255) NOT NULL,
		name VARCHAR(255) NOT NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_genre (tmdb_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// video_people
	`CREATE TABLE IF NOT EXISTS video_people (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		deleted_at TIMESTAMP NULL,
		tmdb_id VARCHAR(255) NOT NULL,
		type VARCHAR(64) NOT NULL,
		name VARCHAR(255) NOT NULL,
		original_name VARCHAR(255) NULL,
		gender TINYINT UNSIGNED NOT NULL DEFAULT 0,
		description TEXT NULL,
		birthday DATE NULL,
		deathday DATE NULL,
		PRIMARY KEY (id),
		UNIQUE KEY unx_people (tmdb_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

	// playback_activity (for user_usage_stats plugin compatibility)
	`CREATE TABLE IF NOT EXISTS playback_activity (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		date_created TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		user_id VARCHAR(64) NOT NULL,
		item_id VARCHAR(64) NULL,
		item_type VARCHAR(64) NULL,
		item_name VARCHAR(255) NULL,
		play_method VARCHAR(64) NULL,
		client VARCHAR(64) NULL,
		device_name VARCHAR(255) NULL,
		play_duration BIGINT NOT NULL DEFAULT 0,
		pause_duration BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (id),
		KEY idx_pa_user (user_id),
		KEY idx_pa_date (date_created)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// Migrate applies the schema. Safe to call every startup.
func Migrate(d *sql.DB) error {
	for i, stmt := range migrations {
		if _, err := d.Exec(stmt); err != nil {
			return fmt.Errorf("migration #%d failed: %w", i, err)
		}
	}
	return nil
}
