package main

import (
	"database/sql"
	"errors"
	"time"
)

type ConflictDraft struct {
	Scope, SessionID string
	Position         Position
	Updated          int64
}

// A revoked connection may still persist its own unsent draft during teardown.
// It cannot choose a session belonging to a different account.
func (s *Store) SaveConflictDraft(user, scope, sessionID string, p Position) error {
	if user == "" || scope == "" || sessionID == "" {
		return errors.New("无效草稿作用域")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRow(`SELECT user FROM sessions WHERE id=?`, sessionID).Scan(&owner); err != nil || owner != user {
			return errors.New("草稿会话不属于当前账户")
		}
		return changedOne(tx.Exec(`INSERT INTO draft_conflicts(user,scope,session_id,draft,reply_to,version,updated) VALUES(?,?,?,?,?,?,?) ON CONFLICT(user,scope,session_id) DO UPDATE SET draft=excluded.draft,reply_to=excluded.reply_to,version=excluded.version,updated=excluded.updated`, user, scope, sessionID, cleanText(p.Draft, 1600), p.ReplyTo, p.Version, time.Now().Unix()))
	})
}

func (s *Store) ConflictDraft(user, scope, sessionID string) (Position, bool, error) {
	var p Position
	err := s.readDB().QueryRow(`SELECT draft,reply_to,version FROM draft_conflicts WHERE user=? AND scope=? AND session_id=?`, user, scope, sessionID).Scan(&p.Draft, &p.ReplyTo, &p.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Store) ConflictDrafts(user string) ([]ConflictDraft, error) {
	rows, err := s.readDB().Query(`SELECT scope,session_id,draft,reply_to,version,updated FROM draft_conflicts WHERE user=? ORDER BY updated DESC,scope,session_id LIMIT 100`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ConflictDraft
	for rows.Next() {
		var draft ConflictDraft
		if err := rows.Scan(&draft.Scope, &draft.SessionID, &draft.Position.Draft, &draft.Position.ReplyTo, &draft.Position.Version, &draft.Updated); err != nil {
			return nil, err
		}
		result = append(result, draft)
	}
	return result, rows.Err()
}

func (s *Store) RemoveConflictDraft(user, scope, sessionID string) error {
	_, err := s.db.Exec(`DELETE FROM draft_conflicts WHERE user=? AND scope=? AND session_id=?`, user, scope, sessionID)
	return err
}
