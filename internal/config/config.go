// Package config loads and holds all runtime configuration from environment variables.
// Mirrors emya (src/env.d.ts) as the reference implementation.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	AppName       string
	AppAuthNumber int
	AppLogLevel   string
	StorageType   string

	ServerHost string
	ServerPort int

	DBDriver          string
	DBHost            string
	DBPort            int
	DBDatabase        string
	DBUsername        string
	DBPassword        string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration

	APIKey      string
	APIExternal string

	// TMDBAPIKey enables automatic metadata backfill. Accepts either a v3
	// API key (32 hex chars) or a v4 bearer token (eyJ...). Leave empty to
	// disable TMDB integration entirely.
	TMDBAPIKey string
	// TMDBLanguage overrides the response language (default: zh-CN).
	TMDBLanguage string
	// TMDBAutoScrape controls whether the library scanner automatically
	// scrapes TMDB metadata for each newly-imported item.
	TMDBAutoScrape bool

	SearchDefaultList string

	MediaProbeWorkers       int
	MediaProbeMaxWorkers    int
	WatchIntervalSeconds    int
	WatchMinIntervalSeconds int

	EmbyVersion          string
	EmbyID               string
	EmbyExtServerDomains string
}

// Load reads configuration from environment variables.
func Load() *Config {
	storageType := normalizeStorageType(getEnv("STORAGE_TYPE", "hdd"))
	maxOpen := getEnvInt("DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns(storageType))
	maxIdle := getEnvInt("DB_MAX_IDLE_CONNS", defaultDBMaxIdleConns(storageType))
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	probeWorkers := getEnvInt("MEDIA_PROBE_WORKERS", defaultMediaProbeWorkers(storageType))
	probeMaxWorkers := getEnvInt("MEDIA_PROBE_MAX_WORKERS", defaultMediaProbeMaxWorkers(storageType))
	if probeWorkers < 1 {
		probeWorkers = 1
	}
	if probeMaxWorkers < probeWorkers {
		probeMaxWorkers = probeWorkers
	}
	watchInterval := getEnvInt("WATCH_INTERVAL_SECONDS", defaultWatchInterval(storageType))
	watchMinInterval := getEnvInt("WATCH_MIN_INTERVAL_SECONDS", defaultWatchMinInterval(storageType))
	if watchMinInterval < 1 {
		watchMinInterval = 1
	}
	if watchInterval < watchMinInterval {
		watchInterval = watchMinInterval
	}

	return &Config{
		AppName:       getEnv("APP_NAME", "emotion"),
		AppAuthNumber: getEnvInt("APP_AUTH_NUMBER", 10),
		AppLogLevel:   getEnv("APP_LOG_LEVEL", "info"),
		StorageType:   storageType,

		ServerHost: getEnv("SERVER_HOST", "0.0.0.0"),
		ServerPort: getEnvInt("SERVER_PORT", 8096),

		DBDriver:          getEnv("DB_DRIVER", "postgres"),
		DBHost:            getEnv("DB_HOST", "127.0.0.1"),
		DBPort:            getEnvInt("DB_PORT", 5432),
		DBDatabase:        getEnv("DB_DATABASE", "emotion"),
		DBUsername:        getEnv("DB_USERNAME", "emotion"),
		DBPassword:        getEnv("DB_PASSWORD", ""),
		DBMaxOpenConns:    maxOpen,
		DBMaxIdleConns:    maxIdle,
		DBConnMaxLifetime: time.Duration(getEnvInt("DB_CONN_MAX_LIFETIME_MINUTES", 30)) * time.Minute,

		APIKey:      getEnv("API_KEY", ""),
		APIExternal: getEnv("API_EXTERNAL", ""),

		TMDBAPIKey:     getEnv("TMDB_API_KEY", ""),
		TMDBLanguage:   getEnv("TMDB_LANGUAGE", "zh-CN"),
		TMDBAutoScrape: strings.EqualFold(getEnv("TMDB_AUTO_SCRAPE", "true"), "true"),

		SearchDefaultList: getEnv("SEARCH_DEFAULT_LIST", `{"欢迎来到 emotion":1}`),

		MediaProbeWorkers:       probeWorkers,
		MediaProbeMaxWorkers:    probeMaxWorkers,
		WatchIntervalSeconds:    watchInterval,
		WatchMinIntervalSeconds: watchMinInterval,

		EmbyVersion:          getEnv("EMBY_VERSION", "4.8.10.0"),
		EmbyID:               getEnv("EMBY_ID", "emotion"),
		EmbyExtServerDomains: getEnv("EMBY_EXT_SERVER_DOMAINS", ""),
	}
}

// DSN returns the PostgreSQL Data Source Name for this configuration.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=disable",
		url.QueryEscape(c.DBUsername), url.QueryEscape(c.DBPassword), c.DBHost, c.DBPort, c.DBDatabase,
	)
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// OptimizeForHDD reports whether IO-heavy jobs should favor low random IO over throughput.
func (c *Config) OptimizeForHDD() bool {
	return c != nil && c.StorageType == "hdd"
}

func normalizeStorageType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ssd", "nvme", "solidstate", "solid-state":
		return "ssd"
	case "hdd", "harddisk", "hard-disk", "disk", "mechanical":
		return "hdd"
	default:
		return "hdd"
	}
}

func defaultDBMaxOpenConns(storageType string) int {
	if storageType == "hdd" {
		return 30
	}
	return 100
}

func defaultDBMaxIdleConns(storageType string) int {
	if storageType == "hdd" {
		return 10
	}
	return 25
}

func defaultMediaProbeWorkers(storageType string) int {
	if storageType == "hdd" {
		return 2
	}
	return 8
}

func defaultMediaProbeMaxWorkers(storageType string) int {
	if storageType == "hdd" {
		return 4
	}
	return 32
}

func defaultWatchInterval(storageType string) int {
	if storageType == "hdd" {
		return 180
	}
	return 30
}

func defaultWatchMinInterval(storageType string) int {
	if storageType == "hdd" {
		return 60
	}
	return 5
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
