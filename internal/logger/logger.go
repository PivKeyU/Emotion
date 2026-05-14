// Package logger provides a thin wrapper around slog for consistent logging.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Entry is one captured backend log record shown in the admin UI.
type Entry struct {
	Time     time.Time         `json:"time"`
	Level    string            `json:"level"`
	Category string            `json:"category,omitempty"`
	Message  string            `json:"message"`
	Attrs    map[string]string `json:"attrs,omitempty"`
}

var defaultBuffer = newBuffer(1000)

// New returns a slog.Logger configured at the requested level.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := &captureHandler{
		next: slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
		buf:  defaultBuffer,
	}
	return slog.New(h)
}

// Recent returns the newest captured logs, filtered by level/category when set.
func Recent(level, category string, limit int) []Entry {
	return defaultBuffer.recent(level, category, limit)
}

type ringBuffer struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
}

func newBuffer(size int) *ringBuffer {
	return &ringBuffer{entries: make([]Entry, size)}
}

func (b *ringBuffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return
	}
	b.entries[b.next] = e
	b.next = (b.next + 1) % len(b.entries)
	if b.next == 0 {
		b.full = true
	}
}

func (b *ringBuffer) recent(level, category string, limit int) []Entry {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	category = strings.ToLower(strings.TrimSpace(category))

	b.mu.Lock()
	defer b.mu.Unlock()

	count := b.next
	if b.full {
		count = len(b.entries)
	}
	capHint := limit
	if count < capHint {
		capHint = count
	}
	out := make([]Entry, 0, capHint)
	for i := 0; i < count && len(out) < limit; i++ {
		idx := b.next - 1 - i
		if idx < 0 {
			idx += len(b.entries)
		}
		e := b.entries[idx]
		if e.Time.IsZero() {
			continue
		}
		if level != "" && strings.ToUpper(e.Level) != level {
			continue
		}
		if category != "" && strings.ToLower(e.Category) != category {
			continue
		}
		out = append(out, e)
	}
	return out
}

type captureHandler struct {
	next  slog.Handler
	buf   *ringBuffer
	attrs []slog.Attr
	group string
}

func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *captureHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.buf != nil {
		attrs := map[string]string{}
		category := ""
		collect := func(a slog.Attr) {
			if a.Key == "" {
				return
			}
			a.Value = a.Value.Resolve()
			key := a.Key
			if h.group != "" {
				key = h.group + "." + key
			}
			value := a.Value.String()
			attrs[key] = value
			if a.Key == "category" {
				category = strings.ToLower(value)
			}
		}
		for _, a := range h.attrs {
			collect(a)
		}
		rec.Attrs(func(a slog.Attr) bool {
			collect(a)
			return true
		})
		if category == "" {
			category = "app"
		}
		h.buf.add(Entry{
			Time:     rec.Time,
			Level:    rec.Level.String(),
			Category: category,
			Message:  rec.Message,
			Attrs:    attrs,
		})
	}
	return h.next.Handle(ctx, rec)
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	cp.next = h.next.WithAttrs(attrs)
	return &cp
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	cp := *h
	cp.next = h.next.WithGroup(name)
	if cp.group == "" {
		cp.group = name
	} else {
		cp.group += "." + name
	}
	return &cp
}
