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

	ValkeyHost     string
	ValkeyPort     int
	ValkeyUsername string
	ValkeyPassword string

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

	EmbyVersion          string
	EmbyID               string
	EmbyExtServerDomains string
}

// Load reads configuration from environment variables.
func Load() *Config {
	maxOpen := getEnvInt("DB_MAX_OPEN_CONNS", 100)
	maxIdle := getEnvInt("DB_MAX_IDLE_CONNS", 25)
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}

	return &Config{
		AppName:       getEnv("APP_NAME", "emotion"),
		AppAuthNumber: getEnvInt("APP_AUTH_NUMBER", 10),
		AppLogLevel:   getEnv("APP_LOG_LEVEL", "info"),

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

		ValkeyHost:     getEnv("VALKEY_HOST", ""),
		ValkeyPort:     getEnvInt("VALKEY_PORT", 6379),
		ValkeyUsername: getEnv("VALKEY_USERNAME", ""),
		ValkeyPassword: getEnv("VALKEY_PASSWORD", ""),

		APIKey:      getEnv("API_KEY", ""),
		APIExternal: getEnv("API_EXTERNAL", ""),

		TMDBAPIKey:     getEnv("TMDB_API_KEY", ""),
		TMDBLanguage:   getEnv("TMDB_LANGUAGE", "zh-CN"),
		TMDBAutoScrape: strings.EqualFold(getEnv("TMDB_AUTO_SCRAPE", "true"), "true"),

		SearchDefaultList: getEnv("SEARCH_DEFAULT_LIST", `{"欢迎来到 emotion":1}`),

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

// ValkeyAddr returns an address usable for go-redis, or empty if cache is disabled.
func (c *Config) ValkeyAddr() string {
	if c.ValkeyHost == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", c.ValkeyHost, c.ValkeyPort)
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
