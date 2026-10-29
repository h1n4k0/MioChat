package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// The error text does not contain SQL/paths or user content. A commit error is
// conservatively uncertain; clients must inspect the object before retrying.
type communityCommitError struct{ cause error }

func (e communityCommitError) Error() string {
	return "提交结果待确认；请先刷新相关条目，勿直接重复创建或评论"
}
func (e communityCommitError) Unwrap() error { return e.cause }

func (s *Store) communityWriteContext(ctx context.Context, user, sessionID string, operation func(queryer) error) error {
	if ctx == nil {
		return errCredentialsChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if sessionID != "" {
		if valid, e := sessionCurrentQ(q, sessionID, user); e != nil || !valid {
			return errCredentialsChanged
		}
		if _, err = mutationActorQ(q, user); err != nil {
			return err
		}
	}
	if err = operation(q); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return communityCommitError{cause: err}
	}
	return nil // Never reinterpret a successfully committed operation as canceled.
}

func newObjectQ(q queryer, user, room, kind, title, payload string) (int64, error) {
	if err := canSendQ(q, user, room, ""); err != nil {
		return 0, err
	}
	title = cleanText(title, 100)
	if title == "" {
		return 0, errors.New("标题不能为空")
	}
	if len(payload) > maxCommunityPayloadBytes {
		return 0, errors.New("内容过长")
	}
	switch kind {
	case "poll", "post", "event", "art", "ttt", "guess", "word":
	default:
		return 0, errors.New("无效社区内容类型")
	}
	if kind == "guess" || kind == "word" {
		var count int
		if err := q.QueryRow(`SELECT count(*) FROM objects WHERE room=? AND kind=? AND closed=0`, room, kind).Scan(&count); err != nil {
			return 0, err
		}
		if count > 0 {
			return 0, errors.New("此频道已有进行中的同类游戏")
		}
	}
	result, err := q.Exec(`INSERT INTO objects(kind,room,owner,title,payload,created) VALUES(?,?,?,?,?,?)`, kind, room, user, title, payload, time.Now().Unix())
	if err = changedOne(result, err); err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if kind == "event" {
		if _, err = q.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'组织者',?)`, user, time.Now().Unix()); err != nil {
			return 0, err
		}
	}
	return id, nil
}
func pollInput(text string) (string, string, error) {
	parts := strings.Split(text, "|")
	if len(parts) < 3 || len(parts) > 11 {
		return "", "", errors.New("用法：/poll 问题|选项1|选项2（2–10 个选项）")
	}
	var options []string
	for _, value := range parts[1:] {
		value = cleanText(value, 80)
		if value == "" {
			return "", "", errors.New("选项不能为空")
		}
		options = append(options, value)
	}
	data, err := json.Marshal(Poll{options})
	return parts[0], string(data), err
}
func eventInput(text string, now time.Time) (string, string, error) {
	parts := strings.SplitN(text, "|", 3)
	if len(parts) < 2 {
		return "", "", errors.New("用法：/event 标题|2026-12-01 20:00|介绍")
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(parts[1]), time.FixedZone("CST", 8*3600))
	if err != nil {
		return "", "", errors.New("时间格式为 YYYY-MM-DD HH:MM（北京时间）")
	}
	if at.Before(now) {
		return "", "", errors.New("活动时间须在未来")
	}
	description := ""
	if len(parts) == 3 {
		description = cleanText(parts[2], 800)
	}
	data, err := json.Marshal(EventData{at.Unix(), description})
	return parts[0], string(data), err
}
func voteQ(q queryer, user string, id int64, choice int) error {
	object, err := objectQ(q, id)
	if err != nil || !canReadQ(q, user, object.Room) || object.Kind != "poll" {
		return errors.New("投票不存在或没有权限")
	}
	if object.Closed {
		return errors.New("投票已截止")
	}
	if err = canSendQ(q, user, object.Room, ""); err != nil {
		return err
	}
	var poll Poll
	if err = json.Unmarshal([]byte(object.Payload), &poll); err != nil {
		return err
	}
	if choice < 1 || choice > len(poll.Options) {
		return errors.New("选项编号超出范围")
	}
	_, err = q.Exec(`INSERT INTO participation VALUES(?,?,?,?) ON CONFLICT(object_id,user) DO UPDATE SET value=excluded.value,at=excluded.at`, id, user, strconv.Itoa(choice), time.Now().Unix())
	return err
}
func rsvpQ(q queryer, user string, id int64, join bool) error {
	object, err := objectQ(q, id)
	if err != nil || object.Kind != "event" || !canReadQ(q, user, object.Room) {
		return errors.New("活动不存在或无权查看")
	}
	if object.Closed {
		return errors.New("活动已结束")
	}
	var data EventData
	if err = json.Unmarshal([]byte(object.Payload), &data); err != nil {
		return err
	}
	if data.When < time.Now().Unix() {
		return errors.New("报名已截止")
	}
	// RSVP is a readable-room participation action, not a chat send. Do not
	// accidentally impose room mute/readonly/slow-mode send restrictions here.
	if join {
		_, err = q.Exec(`INSERT OR IGNORE INTO participation VALUES(?,?,'yes',?)`, id, user, time.Now().Unix())
	} else {
		_, err = q.Exec(`DELETE FROM participation WHERE object_id=? AND user=?`, id, user)
	}
	if err != nil {
		return err
	}
	if join {
		_, err = q.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'参与者',?)`, user, time.Now().Unix())
	}
	return err
}
func commentQ(q queryer, user string, id int64, text string) error {
	object, err := objectQ(q, id)
	if err != nil && !errors.Is(err, ErrObjectTooLarge) || !canReadQ(q, user, object.Room) {
		return errors.New("内容不存在或没有权限")
	}
	if err != nil {
		return err
	}
	if object.Closed {
		return errors.New("此条目已关闭")
	}
	if err = canSendQ(q, user, object.Room, ""); err != nil {
		return err
	}
	text = cleanText(text, 800)
	if text == "" {
		return errors.New("评论不能为空")
	}
	_, err = q.Exec(`INSERT INTO object_events(object_id,user,body,at) VALUES(?,?,?,?)`, id, user, text, time.Now().Unix())
	return err
}
func closeObjectQ(q queryer, user string, id int64) error {
	object, err := objectMetadataQ(q, id)
	if err != nil || !canReadQ(q, user, object.Room) {
		return errors.New("内容不存在或没有权限")
	}
	if user != object.Owner && !canManageQ(q, user, object.Room) {
		return errors.New("仅发起者或频道管理者可结束活动")
	}
	_, err = q.Exec(`UPDATE objects SET closed=1 WHERE id=?`, id)
	return err
}

type communityMutation struct {
	command, room, text string
	expectedRoom        string
	objectID            int64
	choice              int
}

// The only native write entry. An empty session must never take the TUI path.
func (s *Store) communityCommitForSessionContext(ctx context.Context, user, sessionID string, mutation communityMutation) (guiCommunityReceipt, error) {
	if ctx == nil || strings.TrimSpace(sessionID) == "" {
		return guiCommunityReceipt{}, errCredentialsChanged
	}
	id := mutation.objectID
	actualRoom := mutation.room
	err := s.communityWriteContext(ctx, user, sessionID, func(q queryer) error {
		var err error
		if mutation.command != "/poll" && mutation.command != "/event" {
			object, e := objectMetadataQ(q, id)
			if e != nil || !canReadQ(q, user, object.Room) {
				return errors.New("内容不存在或没有权限")
			}
			actualRoom = object.Room
		}
		// This is a comparison against a server-resolved destination, never an ACL grant.
		if mutation.expectedRoom != "" && mutation.expectedRoom != actualRoom {
			return errors.New("原目标频道已变化，请重新打开并核对；未提交")
		}
		switch mutation.command {
		case "/poll", "/event":
			var title, payload string
			if mutation.command == "/poll" {
				title, payload, err = pollInput(mutation.text)
			} else {
				title, payload, err = eventInput(mutation.text, time.Now())
			}
			if err != nil {
				return err
			}
			kind := "poll"
			if mutation.command == "/event" {
				kind = "event"
			}
			id, err = newObjectQ(q, user, mutation.room, kind, title, payload)
			return err
		case "/vote":
			return voteQ(q, user, id, mutation.choice)
		case "/rsvp", "/unrsvp":
			return rsvpQ(q, user, id, mutation.command == "/rsvp")
		case "/comment":
			return commentQ(q, user, id, mutation.text)
		case "/close":
			return closeObjectQ(q, user, id)
		default:
			return errors.New("未知社区写操作")
		}
	})
	if err != nil {
		return guiCommunityReceipt{}, err
	}
	return guiCommunityReceipt{Command: mutation.command, ObjectID: id, Room: actualRoom, Committed: true}, nil
}

// Retains the compact Store API for internal callers/tests, without inferring
// commit from an allocated ID. Production GUI consumes the structured receipt.
func (s *Store) CommunityForSessionContext(ctx context.Context, user, sessionID string, mutation communityMutation) (int64, error) {
	receipt, err := s.communityCommitForSessionContext(ctx, user, sessionID, mutation)
	return receipt.ObjectID, err
}
