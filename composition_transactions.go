package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Snapshot versions include absent-row tombstones. Version zero is a real
// never-written draft, not an omitted precondition or an authorization token.
type CompositionSnapshot struct {
	Owner   string `json:"owner"`
	Room    string `json:"room"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Version int64  `json:"version"`
	Exists  bool   `json:"exists"`
}

var ErrCompositionConflict = errors.New("另一编辑器已更新原频道草稿；本地内容仍保留，请明确重载或选择保留本地")

func compositionKey(user, room, kind string) error {
	if user == "" || room == "" || strings.HasPrefix(room, "dm:") || (kind != "post" && kind != "art") {
		return errors.New("无效的原频道编辑器")
	}
	return nil
}

func compositionVersionQ(q queryer, user, room, kind string) (bool, int64, error) {
	var exists bool
	var version int64
	err := q.QueryRow(`SELECT c.user IS NOT NULL,COALESCE(v.version,0) FROM (SELECT ? AS user,? AS room,? AS kind) k LEFT JOIN compositions c ON c.user=k.user AND c.room=k.room AND c.kind=k.kind LEFT JOIN composition_versions v ON v.user=k.user AND v.room=k.room AND v.kind=k.kind`, user, room, kind).Scan(&exists, &version)
	if err != nil {
		return false, 0, err
	}
	if version < 0 || (exists && version == 0) {
		return false, 0, errors.New("草稿版本状态不可用")
	}
	return exists, version, nil
}

// This projection is used only after current room readability is checked in
// the same transaction. The suspended-save path never reads a remote body.
func compositionSnapshotQ(q queryer, user, room, kind string) (CompositionSnapshot, error) {
	out := CompositionSnapshot{Owner: user, Room: room, Kind: kind}
	var err error
	out.Exists, out.Version, err = compositionVersionQ(q, user, room, kind)
	if err != nil || !out.Exists {
		return out, err
	}
	if err = q.QueryRow(`SELECT title,body FROM compositions WHERE user=? AND room=? AND kind=?`, user, room, kind).Scan(&out.Title, &out.Body); err != nil {
		return CompositionSnapshot{}, err
	}
	return out, nil
}

func (s *Store) CompositionForSession(ctx context.Context, user, sessionID, room, kind string) (CompositionSnapshot, error) {
	if ctx == nil || sessionID == "" {
		return CompositionSnapshot{}, errCredentialsChanged
	}
	if err := compositionKey(user, room, kind); err != nil {
		return CompositionSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	tx, err := s.readDB().BeginTx(ctx, nil)
	if err != nil {
		return CompositionSnapshot{}, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if valid, e := sessionCurrentQ(q, sessionID, user); e != nil || !valid {
		return CompositionSnapshot{}, errCredentialsChanged
	}
	if _, err = mutationActorQ(q, user); err != nil {
		return CompositionSnapshot{}, err
	}
	if !canReadQ(q, user, room) {
		return CompositionSnapshot{}, errors.New("无权访问原频道")
	}
	out, err := compositionSnapshotQ(q, user, room, kind)
	if err != nil {
		return CompositionSnapshot{}, err
	}
	if err = tx.Commit(); err != nil {
		return CompositionSnapshot{}, err
	}
	return out, nil
}

func compositionSaveQ(q queryer, user, room, kind string, expected int64, title, body string, suspended bool) (CompositionSnapshot, error) {
	out := CompositionSnapshot{Owner: user, Room: room, Kind: kind}
	if err := compositionKey(user, room, kind); err != nil {
		return out, err
	}
	if expected < 0 || expected == 1<<63-1 {
		return out, ErrCompositionConflict
	}
	if !suspended && !canReadQ(q, user, room) {
		return out, errors.New("无权访问原频道")
	}
	exists, version, err := compositionVersionQ(q, user, room, kind)
	if err != nil {
		return out, err
	}
	if version != expected {
		return out, ErrCompositionConflict
	}
	if suspended && !exists {
		return out, errors.New("没有可保留的本人原频道草稿")
	}
	clean, err := multiline(body, false)
	if err != nil {
		return out, err
	}
	title = cleanText(title, 100)
	if exists {
		err = changedOne(q.Exec(`UPDATE compositions SET title=?,body=?,updated=? WHERE user=? AND room=? AND kind=?`, title, clean, time.Now().Unix(), user, room, kind))
	} else {
		err = changedOne(q.Exec(`INSERT INTO compositions VALUES(?,?,?,?,?,?)`, user, room, kind, title, clean, time.Now().Unix()))
	}
	if err != nil {
		return out, err
	}
	// Read only a boolean equality check, not the body, after an ACL loss.
	// This also detects write/trigger IGNORE and rewritten content before commit.
	var matches int
	if err = q.QueryRow(`SELECT count(*) FROM compositions WHERE user=? AND room=? AND kind=? AND title=? AND body=?`, user, room, kind, title, clean).Scan(&matches); err != nil || matches != 1 {
		if err != nil {
			return out, err
		}
		return out, errors.New("原频道草稿未按请求保存")
	}
	exists, actual, err := compositionVersionQ(q, user, room, kind)
	if err != nil {
		return out, err
	}
	if !exists || actual <= expected {
		return out, errors.New("草稿版本未推进")
	}
	out.Title, out.Body, out.Version, out.Exists = title, clean, actual, true
	return out, nil
}

// All real TUI and native writers must supply their current session. The
// existing unversioned Store wrappers are compatibility/fixture-only callers.
func (s *Store) SaveCompositionForSession(ctx context.Context, user, sessionID, room, kind string, expected int64, title, body string, suspended bool) (CompositionSnapshot, error) {
	var out CompositionSnapshot
	if ctx == nil || sessionID == "" {
		return out, errCredentialsChanged
	}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		var err error
		out, err = compositionSaveQ(q, user, room, kind, expected, title, body, suspended)
		return err
	})
	if err != nil {
		return CompositionSnapshot{}, err
	}
	return out, nil
}

func (s *Store) ReserveCompositionForSession(ctx context.Context, user, sessionID, room, kind string, expected int64, requestedTitle string) (CompositionSnapshot, error) {
	var out CompositionSnapshot
	if ctx == nil || sessionID == "" {
		return out, errCredentialsChanged
	}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		if err := compositionKey(user, room, kind); err != nil {
			return err
		}
		if !canReadQ(q, user, room) {
			return errors.New("无权访问原频道")
		}
		current, err := compositionSnapshotQ(q, user, room, kind)
		if err != nil {
			return err
		}
		if expected < 0 || current.Version != expected {
			return ErrCompositionConflict
		}
		title := cleanText(requestedTitle, 100)
		if requestedTitle == "" && current.Title != "" {
			title = current.Title
		}
		if title == "" {
			title = "未命名作品"
		}
		if current.Exists && title == current.Title {
			out = current
			return nil
		}
		out, err = compositionSaveQ(q, user, room, kind, expected, title, current.Body, false)
		return err
	})
	if err != nil {
		return CompositionSnapshot{}, err
	}
	return out, nil
}

func (s *Store) PublishCompositionForSession(ctx context.Context, user, sessionID, room, kind string, expected int64, title, body string) (int64, CompositionSnapshot, error) {
	var id int64
	var cleared CompositionSnapshot
	if ctx == nil || sessionID == "" {
		return 0, cleared, errCredentialsChanged
	}
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		if err := compositionKey(user, room, kind); err != nil {
			return err
		}
		if !canReadQ(q, user, room) {
			return errors.New("无权访问原频道")
		}
		exists, version, err := compositionVersionQ(q, user, room, kind)
		if err != nil {
			return err
		}
		if !exists || expected < 0 || version != expected {
			return ErrCompositionConflict
		}
		clean, err := multiline(body, kind == "art")
		if err != nil {
			return err
		}
		if strings.TrimSpace(clean) == "" {
			return errors.New("正文不能为空")
		}
		payload, err := json.Marshal(map[string]string{"body": clean})
		if err != nil {
			return err
		}
		id, err = newObjectQ(q, user, room, kind, title, string(payload))
		if err != nil {
			return err
		}
		// Object creation and exact-version clearing commit together. A clear
		// IGNORE/error, lost version or session revocation can never leave an
		// object while pretending its draft was cleared.
		cleared, err = compositionSaveQ(q, user, room, kind, expected, title, "", false)
		if err != nil {
			return err
		}
		stored, err := objectQ(q, id)
		if err != nil {
			return err
		}
		if stored.Owner != user || stored.Room != room || stored.Kind != kind || stored.Title != cleared.Title || stored.Payload != string(payload) || stored.Closed {
			return errors.New("作品未按请求原子发布")
		}
		return nil
	})
	if err != nil {
		return 0, CompositionSnapshot{}, err
	}
	return id, cleared, nil
}
