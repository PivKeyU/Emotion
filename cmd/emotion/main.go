// Package main is the Emotion server entrypoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/PivKeyU/Emotion/internal/cache"
	"github.com/PivKeyU/Emotion/internal/config"
	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/logger"
	"github.com/PivKeyU/Emotion/internal/server"
)

func main() {
	// Best-effort .env load, don't fail if missing.
	_ = godotenv.Load()

	cfg := config.Load()
	log := logger.New(cfg.AppLogLevel)

	log.Info("starting emotion", "name", cfg.AppName, "version", cfg.EmbyVersion)

	database, err := db.Open(cfg)
	if err != nil {
		log.Error("failed to open database", "err", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		log.Error("failed to migrate database", "err", err)
		os.Exit(1)
	}

	cacheStore := cache.New()

	deps := &server.Dependencies{
		Config: cfg,
		DB:     database,
		Cache:  cacheStore,
		Logger: log,
	}

	handler := server.NewRouter(deps)

	addr := fmt.Sprintf("%s:%d", cfg.ServerHost, cfg.ServerPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		log.Info("emotion running", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down gracefully")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
}
