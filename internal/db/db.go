// Package db manages the PostgreSQL connection pool and query compatibility.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/PivKeyU/Emotion/internal/config"
)

// DB wraps sql.DB and rewrites legacy question-mark placeholders to PostgreSQL
// placeholders. That keeps handler code compact while the backing store is
// PostgreSQL.
type DB struct {
	*sql.DB
}

type result struct {
	lastID       int64
	rowsAffected int64
}

func (r result) LastInsertId() (int64, error) { return r.lastID, nil }
func (r result) RowsAffected() (int64, error) { return r.rowsAffected, nil }

// Open opens the configured PostgreSQL database and verifies connectivity.
func Open(cfg *config.Config) (*DB, error) {
	driver := strings.ToLower(cfg.DBDriver)
	if driver != "postgres" && driver != "postgresql" && driver != "pgx" {
		return nil, fmt.Errorf("unsupported db driver: %s (only postgres is supported)", cfg.DBDriver)
	}

	d, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	d.SetMaxOpenConns(cfg.DBMaxOpenConns)
	d.SetMaxIdleConns(cfg.DBMaxIdleConns)
	d.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	d.SetConnMaxIdleTime(5 * time.Minute)

	if err := d.Ping(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}

	return &DB{DB: d}, nil
}

// ExecContext rewrites positional placeholders before executing the statement.
// INSERT statements are executed with RETURNING id so legacy LastInsertId calls
// keep working on PostgreSQL.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if isInsert(query) && !hasReturning(query) && !hasDoNothing(query) {
		var id int64
		q := appendReturningID(query)
		if err := d.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			return nil, err
		}
		return result{lastID: id, rowsAffected: 1}, nil
	}
	res, err := d.DB.ExecContext(ctx, Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// QueryContext rewrites positional placeholders before querying.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, Rebind(query), args...)
}

// QueryRowContext rewrites positional placeholders before querying one row.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, Rebind(query), args...)
}

// InsertID runs an INSERT ... RETURNING id statement and returns the generated id.
func (d *DB) InsertID(ctx context.Context, query string, args ...any) (int64, error) {
	var id int64
	err := d.QueryRowContext(ctx, appendReturningID(query), args...).Scan(&id)
	return id, err
}

func isInsert(query string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "INSERT ")
}

func hasReturning(query string) bool {
	return strings.Contains(strings.ToUpper(query), " RETURNING ")
}

func hasDoNothing(query string) bool {
	return strings.Contains(strings.ToUpper(query), " DO NOTHING")
}

func appendReturningID(query string) string {
	q := strings.TrimSpace(query)
	q = strings.TrimSuffix(q, ";")
	if hasReturning(q) {
		return q
	}
	return q + " RETURNING id"
}

// Rebind converts unquoted ? placeholders into PostgreSQL $n placeholders.
func Rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	arg := 1
	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(query); i++ {
		ch := query[i]
		next := byte(0)
		if i+1 < len(query) {
			next = query[i+1]
		}

		if inLineComment {
			b.WriteByte(ch)
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			b.WriteByte(ch)
			if ch == '*' && next == '/' {
				i++
				b.WriteByte(next)
				inBlockComment = false
			}
			continue
		}

		if !inSingle && !inDouble {
			if ch == '-' && next == '-' {
				b.WriteByte(ch)
				i++
				b.WriteByte(next)
				inLineComment = true
				continue
			}
			if ch == '/' && next == '*' {
				b.WriteByte(ch)
				i++
				b.WriteByte(next)
				inBlockComment = true
				continue
			}
		}

		switch ch {
		case '\'':
			b.WriteByte(ch)
			if inSingle && next == '\'' {
				i++
				b.WriteByte(next)
				continue
			}
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			b.WriteByte(ch)
			if !inSingle {
				inDouble = !inDouble
			}
		case '?':
			if inSingle || inDouble {
				b.WriteByte(ch)
				continue
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(arg))
			arg++
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}
