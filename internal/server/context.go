// Package server wires the HTTP router, middleware, and handlers together.
package server

import (
	"database/sql"
	"log/slog"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
)

// Dependencies is the set of shared services injected into handlers.
type Dependencies struct {
	Config *config.Config
	DB     *sql.DB
	Cache  cache.Cache
	Logger *slog.Logger
}
