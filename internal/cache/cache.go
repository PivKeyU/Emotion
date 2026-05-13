// Package cache provides a tiny key-value cache interface with an in-memory
// implementation. A Valkey/Redis backend can be added later (the interface is
// ready); for this build we ship memory-only to keep dependencies minimal.
package cache

import (
	"context"
	"sync"
	"time"

	"github.com/PivKeyU/Emotion/internal/config"
)

// Cache is the minimal interface used by the server.
type Cache interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, value string, ttl time.Duration)
	Delete(ctx context.Context, key string)
}

// New returns the default cache. Currently in-memory only.
// Valkey/Redis is TODO - not critical since all call-sites are idempotent.
func New(cfg *config.Config) Cache {
	_ = cfg
	return newMemoryCache()
}

type memoryEntry struct {
	value   string
	expires time.Time
}

type memoryCache struct {
	mu    sync.RWMutex
	store map[string]memoryEntry
}

func newMemoryCache() *memoryCache {
	c := &memoryCache{store: make(map[string]memoryEntry)}
	go c.gc()
	return c
}

func (c *memoryCache) Get(_ context.Context, key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.store[key]
	if !ok || (!e.expires.IsZero() && time.Now().After(e.expires)) {
		return "", false
	}
	return e.value, true
}

func (c *memoryCache) Set(_ context.Context, key, value string, ttl time.Duration) {
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[key] = memoryEntry{value: value, expires: exp}
}

func (c *memoryCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.store, key)
}

func (c *memoryCache) gc() {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		now := time.Now()
		c.mu.Lock()
		for k, v := range c.store {
			if !v.expires.IsZero() && now.After(v.expires) {
				delete(c.store, k)
			}
		}
		c.mu.Unlock()
	}
}
