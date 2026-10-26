package main

import (
	"context"
	"database/sql"
)

type contextDatabase interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type scopedQuery struct {
	ctx context.Context
	db  contextDatabase
}

func (q scopedQuery) QueryRow(query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(q.ctx, query, args...)
}
func (q scopedQuery) Query(query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(q.ctx, query, args...)
}
func (q scopedQuery) Exec(query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(q.ctx, query, args...)
}
