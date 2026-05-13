// Package server wires the HTTP router, middleware, and handlers together.
package server

import (
	"log/slog"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
)

// Dependencies is the set of shared services injected into handlers.
type Dependencies struct {
	Config *config.Config
	DB     *db.DB
	Cache  cache.Cache
	Logger *slog.Logger
}
