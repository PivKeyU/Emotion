// Package cache provides a tiny key-value cache interface with in-memory and
// optional Valkey/Redis-backed implementations.
package cache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
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

// New returns the default cache. When Valkey is configured it is used as a
// shared cache, with an in-process cache kept as a fast fallback.
func New(cfg *config.Config) Cache {
	local := newMemoryCache()
	if cfg == nil || cfg.ValkeyAddr() == "" {
		return local
	}
	return &tieredCache{
		local:  local,
		remote: newValkeyCache(cfg),
	}
}

type tieredCache struct {
	local  Cache
	remote Cache
}

func (c *tieredCache) Get(ctx context.Context, key string) (string, bool) {
	if value, ok := c.local.Get(ctx, key); ok {
		return value, true
	}
	value, ok := c.remote.Get(ctx, key)
	if ok {
		c.local.Set(ctx, key, value, 30*time.Second)
	}
	return value, ok
}

func (c *tieredCache) Set(ctx context.Context, key, value string, ttl time.Duration) {
	c.local.Set(ctx, key, value, ttl)
	c.remote.Set(ctx, key, value, ttl)
}

func (c *tieredCache) Delete(ctx context.Context, key string) {
	c.local.Delete(ctx, key)
	c.remote.Delete(ctx, key)
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

type valkeyCache struct {
	addr     string
	username string
	password string
	timeout  time.Duration
}

func newValkeyCache(cfg *config.Config) *valkeyCache {
	return &valkeyCache{
		addr:     cfg.ValkeyAddr(),
		username: cfg.ValkeyUsername,
		password: cfg.ValkeyPassword,
		timeout:  250 * time.Millisecond,
	}
}

func (c *valkeyCache) Get(ctx context.Context, key string) (string, bool) {
	reply, err := c.do(ctx, "GET", c.key(key))
	if err != nil {
		return "", false
	}
	value, ok := reply.(string)
	return value, ok
}

func (c *valkeyCache) Set(ctx context.Context, key, value string, ttl time.Duration) {
	if ttl > 0 {
		seconds := int64(ttl.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		_, _ = c.do(ctx, "SET", c.key(key), value, "EX", strconv.FormatInt(seconds, 10))
		return
	}
	_, _ = c.do(ctx, "SET", c.key(key), value)
}

func (c *valkeyCache) Delete(ctx context.Context, key string) {
	_, _ = c.do(ctx, "DEL", c.key(key))
}

func (c *valkeyCache) key(key string) string {
	return "emotion:" + key
}

func (c *valkeyCache) do(ctx context.Context, args ...string) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	r := bufio.NewReader(conn)

	if c.password != "" {
		authArgs := []string{"AUTH", c.password}
		if c.username != "" {
			authArgs = []string{"AUTH", c.username, c.password}
		}
		if err := writeRESPCommand(conn, authArgs...); err != nil {
			return nil, err
		}
		if _, err := readRESP(r); err != nil {
			return nil, err
		}
	}

	if err := writeRESPCommand(conn, args...); err != nil {
		return nil, err
	}
	return readRESP(r)
}

func writeRESPCommand(w io.Writer, args ...string) error {
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(strconv.Itoa(len(args)))
	b.WriteString("\r\n")
	for _, arg := range args {
		b.WriteString("$")
		b.WriteString(strconv.Itoa(len(arg)))
		b.WriteString("\r\n")
		b.WriteString(arg)
		b.WriteString("\r\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return nil, fmt.Errorf("empty valkey response")
	}

	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, fmt.Errorf("valkey error: %s", line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	default:
		return nil, fmt.Errorf("unsupported valkey response: %q", line)
	}
}
