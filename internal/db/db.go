// Package db manages the MySQL connection pool and provides shared helpers.
package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/PivKeyU/Next-Emby/internal/config"
)

// Open opens the configured database and verifies connectivity.
func Open(cfg *config.Config) (*sql.DB, error) {
	if cfg.DBDriver != "mysql" {
		return nil, fmt.Errorf("unsupported db driver: %s (only mysql is supported)", cfg.DBDriver)
	}

	d, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	d.SetMaxOpenConns(cfg.DBMaxOpenConns)
	d.SetMaxIdleConns(cfg.DBMaxOpenConns / 2)
	d.SetConnMaxLifetime(time.Hour)

	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	return d, nil
}
