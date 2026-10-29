package main

import (
	"context"
	"database/sql"
	"sync"
)

// readStatements retains bounded execution plans, never arguments, rows, or
// authorization decisions. Each execution binds fresh parameters and uses the
// caller's original deadline. It does not hold a connection between requests.
type readStatements struct {
	db       *sql.DB
	capacity int
	prepare  chan struct{}
	mu       sync.Mutex
	closed   bool
	items    map[string]*sql.Stmt
}

func newReadStatements(db *sql.DB, capacity int) *readStatements {
	return &readStatements{db: db, capacity: capacity, prepare: make(chan struct{}, 1), items: make(map[string]*sql.Stmt)}
}

func (c *readStatements) lookup(query string) (*sql.Stmt, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, sql.ErrConnDone
	}
	stmt := c.items[query]
	return stmt, stmt == nil && len(c.items) >= c.capacity, nil
}

func (c *readStatements) statement(ctx context.Context, query string) (*sql.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stmt, full, err := c.lookup(query)
	if err != nil || stmt != nil || full {
		return stmt, err // Only a full cache falls back to the original query.
	}
	// The miss gate is deadline-aware; cache hits never wait behind Prepare.
	select {
	case c.prepare <- struct{}{}:
		defer func() { <-c.prepare }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stmt, full, err = c.lookup(query)
	if err != nil || stmt != nil || full {
		return stmt, err
	}
	stmt, err = c.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err // Never turn preparation failure into an implicit retry.
	}
	c.mu.Lock()
	if c.closed || ctx.Err() != nil {
		c.mu.Unlock()
		_ = stmt.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrConnDone
	}
	c.items[query] = stmt
	c.mu.Unlock()
	return stmt, nil
}

func (c *readStatements) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	items := c.items
	c.items = nil
	c.mu.Unlock()
	for _, stmt := range items {
		_ = stmt.Close()
	}
}

func (s *Store) readQueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return cachedReadQuery(ctx, s.readDB(), s.db.readPlans, query, args...)
}

func (s *Store) policyQueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return cachedReadQuery(ctx, s.policyDB(), s.db.policyPlans, query, args...)
}

func cachedReadQuery(ctx context.Context, db *sql.DB, cache *readStatements, query string, args ...any) (*sql.Rows, error) {
	// A substituted/faulted pool must never reuse plans from the previous DB.
	if cache == nil || cache.db != db {
		return db.QueryContext(ctx, query, args...)
	}
	stmt, err := cache.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	if stmt == nil {
		return db.QueryContext(ctx, query, args...)
	}
	return stmt.QueryContext(ctx, args...)
}

// Keep QueryRow's scan/close semantics while preserving a Prepare error.
type preparedReadRow struct {
	row *sql.Row
	err error
}

func (r preparedReadRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

func cachedReadRow(ctx context.Context, db *sql.DB, cache *readStatements, query string, args ...any) preparedReadRow {
	if cache == nil || cache.db != db {
		return preparedReadRow{row: db.QueryRowContext(ctx, query, args...)}
	}
	stmt, err := cache.statement(ctx, query)
	if err != nil {
		return preparedReadRow{err: err}
	}
	if stmt == nil {
		return preparedReadRow{row: db.QueryRowContext(ctx, query, args...)}
	}
	return preparedReadRow{row: stmt.QueryRowContext(ctx, args...)}
}

func (s *Store) readRowContext(ctx context.Context, query string, args ...any) preparedReadRow {
	return cachedReadRow(ctx, s.readDB(), s.db.readPlans, query, args...)
}

func (s *Store) policyRowContext(ctx context.Context, query string, args ...any) preparedReadRow {
	return cachedReadRow(ctx, s.policyDB(), s.db.policyPlans, query, args...)
}
