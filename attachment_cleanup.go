package main

import (
	"context"
	"errors"
	"time"
)

// A bounded internal maintenance batch (<=64 rows and <=20M accepted source
// bytes per batch), not a client operation. It visits all
// owners, including abandoned/disabled accounts. DELETE makes SQLite pages
// reusable; it does not promise a smaller database file or perform VACUUM.
func (s *Store) ExpireAttachmentUploads(ctx context.Context, limit int) (int, error) {
	if ctx == nil || limit < 1 || limit > 64 {
		return 0, errors.New("invalid attachment expiry batch")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if err = changedOne(q.Exec(`UPDATE attachment_writer_guard SET value=value WHERE id=1`)); err != nil {
		return 0, err
	}
	now := s.attachmentClock()
	rows, err := q.Query(`SELECT id,owner_uid,accepted FROM attachment_uploads WHERE state='uploading' AND expires_at<=? ORDER BY expires_at,id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return 0, err
	}
	type key struct {
		id       string
		uid      int64
		accepted int64
	}
	keys := make([]key, 0, limit)
	var bytes int64
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.uid, &k.accepted); err != nil {
			rows.Close()
			return 0, err
		}
		if len(keys) > 0 && bytes+k.accepted > attachmentItemLimit {
			break
		}
		bytes += k.accepted
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, k := range keys {
		u, e := attachmentUploadQ(q, k.uid, k.id, "")
		if e != nil {
			return 0, e
		}
		if e = attachmentFinishStateQ(q, &u, "expired"); e != nil {
			return 0, e
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, attachmentUnknownError{err}
	}
	return len(keys), nil
}

// Reuse the existing server GUI health tick; no unbounded worker or per-client
// cleanup goroutine. Across sessions, only one batch per Store per 30 seconds.
func (s *Store) sweepExpiredAttachments(ctx context.Context) {
	if !s.attachmentSweepMu.TryLock() {
		return
	}
	defer s.attachmentSweepMu.Unlock()
	now := time.Now()
	if !s.attachmentSweepAt.IsZero() && now.Sub(s.attachmentSweepAt) < 30*time.Second {
		return
	}
	if _, err := s.ExpireAttachmentUploads(ctx, 32); err == nil {
		s.attachmentSweepAt = now
	}
}
