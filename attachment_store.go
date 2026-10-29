package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type attachmentError struct{ code string }

func (e attachmentError) Error() string {
	return "附件操作未完成 (" + e.code + ")；请保留原上传编号"
}
func attachmentErr(code string) error { return attachmentError{code} }

type attachmentUnknownError struct{ cause error }

func (e attachmentUnknownError) Error() string {
	return "附件提交结果待确认；请查询原上传编号，不要重新发送"
}
func (e attachmentUnknownError) Unwrap() error { return e.cause }

type attachmentUpload struct {
	info                                                attachmentInfo
	ownerUID, epoch, roomEpoch, recipientUID            int64
	originSession, intentKey, room, caption, day, state string
	expires, accepted, messageID                        int64
}

const attachmentUploadColumns = `id,owner_uid,auth_epoch,origin_session,intent_key,room,room_epoch,recipient_uid,filename,mime,size,sha256,caption,day,expires_at,accepted,state,message_id,width,height,frames`

func attachmentUploadQ(q queryer, uid int64, id, key string) (attachmentUpload, error) {
	var u attachmentUpload
	err := q.QueryRow(`SELECT `+attachmentUploadColumns+` FROM attachment_uploads WHERE owner_uid=? AND ((?!='' AND id=?) OR (?!='' AND intent_key=?))`, uid, id, id, key, key).Scan(&u.info.ID, &u.ownerUID, &u.epoch, &u.originSession, &u.intentKey, &u.room, &u.roomEpoch, &u.recipientUID, &u.info.Filename, &u.info.MIME, &u.info.Size, &u.info.SHA256, &u.caption, &u.day, &u.expires, &u.accepted, &u.state, &u.messageID, &u.info.Width, &u.info.Height, &u.info.Frames)
	u.info.MetadataPolicy = "preserve_original"
	return u, err
}

var attachmentTimezone = time.FixedZone("UTC+08:00", 8*60*60)

func attachmentDay(now time.Time) (string, time.Time) {
	t := now.In(attachmentTimezone)
	return t.Format("2006-01-02"), time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, attachmentTimezone)
}
func (s *Store) attachmentClock() time.Time {
	if s.attachmentNow != nil {
		return s.attachmentNow()
	}
	return time.Now()
}

func attachmentActorQ(q queryer, expected User, session string) (User, error) {
	if session == "" || expected.UID <= 0 {
		return User{}, errCredentialsChanged
	}
	actor, err := mutationActorQ(q, expected.Name)
	if err != nil || actor.UID != expected.UID || actor.AuthEpoch != expected.AuthEpoch {
		return User{}, errCredentialsChanged
	}
	valid, err := sessionCurrentQ(q, session, actor.Name)
	if err != nil || !valid {
		return User{}, errCredentialsChanged
	}
	return actor, nil
}

// Explicit no-op UPDATE obtains the SQLite writer lock before clock capture,
// even with a second Store/process. Deferred BEGIN alone does NOT do this.
func (s *Store) attachmentWrite(ctx context.Context, expected User, session string, op func(queryer, User, time.Time) error) error {
	if ctx == nil || session == "" {
		return errCredentialsChanged
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if err = changedOne(q.Exec(`UPDATE attachment_writer_guard SET value=value WHERE id=1`)); err != nil {
		return err
	}
	actor, err := attachmentActorQ(q, expected, session)
	if err != nil {
		return err
	}
	if err = op(q, actor, s.attachmentClock()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return attachmentUnknownError{err}
	}
	return nil
}

// Resolve identities inside each operation. The room-generation row is deleted
// with its room; recreating the same room name never adopts an old upload.
func attachmentTargetQ(q queryer, actor User, scope string, write, create bool) (int64, int64, error) {
	if scope == "" || len(scope) > 128 {
		return 0, 0, attachmentErr("target_invalid")
	}
	if strings.HasPrefix(scope, "dm:") {
		name := strings.TrimPrefix(scope, "dm:")
		other, err := userQ(q, name)
		if err != nil || name != other.Name || other.UID == actor.UID || other.Banned {
			return 0, 0, attachmentErr("target_unavailable")
		}
		if write {
			if err = canSendQ(q, actor.Name, "", other.Name); err != nil {
				return 0, 0, attachmentErr("target_denied")
			}
		}
		return 0, other.UID, nil
	}
	if !canReadQ(q, actor.Name, scope) {
		return 0, 0, attachmentErr("target_denied")
	}
	if write {
		if err := canSendQ(q, actor.Name, scope, ""); err != nil {
			return 0, 0, attachmentErr("target_denied")
		}
	}
	if create {
		if _, err := q.Exec(`INSERT OR IGNORE INTO attachment_room_epochs(name) VALUES(?)`, scope); err != nil {
			return 0, 0, err
		}
	}
	var epoch int64
	if err := q.QueryRow(`SELECT id FROM attachment_room_epochs WHERE name=?`, scope).Scan(&epoch); err != nil {
		return 0, 0, attachmentErr("target_unavailable")
	}
	return epoch, 0, nil
}

func attachmentAuthorizeQ(q queryer, actor User, session, scope string, u attachmentUpload, write bool) error {
	if actor.UID != u.ownerUID || actor.AuthEpoch != u.epoch {
		return errCredentialsChanged
	}
	if scope != u.room {
		return attachmentErr("target_changed")
	}
	r, p, err := attachmentTargetQ(q, actor, scope, write, false)
	if err != nil {
		return err
	}
	if r != u.roomEpoch || p != u.recipientUID {
		return attachmentErr("target_changed")
	}
	return nil
}

func attachmentQuotaQ(q queryer, uid int64, day string) (attachmentQuota, error) {
	out := attachmentQuota{Day: day, DailyLimit: attachmentDailyLimit, ItemLimit: attachmentItemLimit, Meter: "accepted_source_bytes_plus_reserved"}
	t, err := time.ParseInLocation("2006-01-02", day, attachmentTimezone)
	if err != nil {
		return out, err
	}
	out.ResetAt = t.AddDate(0, 0, 1).Unix()
	err = q.QueryRow(`SELECT used,reserved FROM attachment_quota WHERE owner_uid=? AND day=?`, uid, day).Scan(&out.Used, &out.Reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	return out, err
}

func attachmentQuotaChangeQ(q queryer, uid int64, day string, usedDelta, reservedDelta int64) error {
	before, err := attachmentQuotaQ(q, uid, day)
	if err != nil {
		return err
	}
	if before.Used+usedDelta < 0 || before.Reserved+reservedDelta < 0 || before.Used+usedDelta+before.Reserved+reservedDelta > attachmentDailyLimit {
		return attachmentErr("daily_quota")
	}
	if err = changedOne(q.Exec(`UPDATE attachment_quota SET used=used+?,reserved=reserved+? WHERE owner_uid=? AND day=? AND used=? AND reserved=?`, usedDelta, reservedDelta, uid, day, before.Used, before.Reserved)); err != nil {
		return err
	}
	after, err := attachmentQuotaQ(q, uid, day)
	if err != nil {
		return err
	}
	if after.Used != before.Used+usedDelta || after.Reserved != before.Reserved+reservedDelta {
		return attachmentErr("storage_mismatch")
	}
	return nil
}

func attachmentFinishStateQ(q queryer, u *attachmentUpload, state string) error {
	if u.state != "uploading" {
		return nil
	}
	if err := attachmentQuotaChangeQ(q, u.ownerUID, u.day, 0, -(u.info.Size - u.accepted)); err != nil {
		return err
	}
	if err := changedOne(q.Exec(`UPDATE attachment_uploads SET state=? WHERE id=? AND state='uploading' AND accepted=?`, state, u.info.ID, u.accepted)); err != nil {
		return err
	}
	var actual string
	if err := q.QueryRow(`SELECT state FROM attachment_uploads WHERE id=?`, u.info.ID).Scan(&actual); err != nil {
		return err
	}
	if actual != state {
		return attachmentErr("storage_mismatch")
	}
	if _, err := q.Exec(`DELETE FROM attachment_chunks WHERE upload_id=?`, u.info.ID); err != nil {
		return err
	}
	var count int
	if err := q.QueryRow(`SELECT count(*) FROM attachment_chunks WHERE upload_id=?`, u.info.ID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return attachmentErr("storage_mismatch")
	}
	u.state = state
	return nil
}

func attachmentExpireOwnerQ(q queryer, uid int64, now time.Time) error {
	rows, err := q.Query(`SELECT id FROM attachment_uploads WHERE owner_uid=? AND state='uploading' AND expires_at<=?`, uid, now.Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		u, e := attachmentUploadQ(q, uid, id, "")
		if e != nil {
			return e
		}
		if e = attachmentFinishStateQ(q, &u, "expired"); e != nil {
			return e
		}
	}
	return nil
}

func attachmentReceiptQ(q queryer, u attachmentUpload, op string) (guiAttachmentReceipt, error) {
	quota, err := attachmentQuotaQ(q, u.ownerUID, u.day)
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	out := guiAttachmentReceipt{Op: op, Status: u.state, IntentKey: u.intentKey, UploadID: u.info.ID, Room: u.room, OwnerUID: u.ownerUID, AcceptedOffset: u.accepted, Size: u.info.Size, ExpiresAt: u.expires, MessageID: u.messageID, Quota: &quota}
	info := u.info
	out.Attachment = &info
	return out, nil
}

func attachmentKeyValid(key string) bool {
	if len(key) < 8 || len(key) > 64 {
		return false
	}
	for _, c := range key {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func attachmentIDValid(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && strings.ToLower(id) == id
}

func attachmentIntentMatches(u attachmentUpload, in guiAttachmentRequest, scope string) bool {
	return u.room == scope && u.info.Filename == in.Filename && u.info.MIME == in.MIME && u.info.Size == in.Size && u.info.SHA256 == in.SHA256 && u.caption == in.Caption
}

func attachmentIntentQ(q queryer, actor User, session, scope string, in guiAttachmentRequest, now time.Time) (guiAttachmentReceipt, error) {
	if !attachmentKeyValid(in.IntentKey) || in.Size < 1 || in.Size > attachmentItemLimit || !attachmentFilenameValid(in.Filename, in.MIME) || !attachmentSHAValid(in.SHA256) || len(in.Caption) > 2400 || cleanText(in.Caption, 800) != in.Caption {
		return guiAttachmentReceipt{}, attachmentErr("intent_invalid")
	}
	if _, err := attachmentReferenceIntentQ(q, actor.UID, in.IntentKey); err == nil {
		return guiAttachmentReceipt{}, attachmentErr("intent_conflict")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return guiAttachmentReceipt{}, err
	}
	if old, err := attachmentUploadQ(q, actor.UID, "", in.IntentKey); err == nil {
		if !attachmentIntentMatches(old, in, scope) {
			return guiAttachmentReceipt{}, attachmentErr("intent_conflict")
		}
		if err = attachmentAuthorizeQ(q, actor, session, scope, old, false); err != nil {
			return guiAttachmentReceipt{}, err
		}
		return attachmentReceiptQ(q, old, in.Op)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return guiAttachmentReceipt{}, err
	}
	r, p, err := attachmentTargetQ(q, actor, scope, true, true)
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	day, midnight := attachmentDay(now)
	var count int
	if err = q.QueryRow(`SELECT count(*) FROM attachment_uploads WHERE owner_uid=? AND state='uploading'`, actor.UID).Scan(&count); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if count >= attachmentPendingLimit {
		return guiAttachmentReceipt{}, attachmentErr("pending_limit")
	}
	if err = q.QueryRow(`SELECT count(*) FROM attachment_uploads WHERE owner_uid=? AND day=?`, actor.UID, day).Scan(&count); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if count >= 128 {
		return guiAttachmentReceipt{}, attachmentErr("intent_limit")
	}
	if _, err = q.Exec(`INSERT OR IGNORE INTO attachment_quota(owner_uid,day) VALUES(?,?)`, actor.UID, day); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if err = attachmentQuotaChangeQ(q, actor.UID, day, 0, in.Size); err != nil {
		return guiAttachmentReceipt{}, err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return guiAttachmentReceipt{}, err
	}
	id := hex.EncodeToString(random[:])
	expires := now.Add(attachmentTTL)
	if midnight.Before(expires) {
		expires = midnight
	}
	if err = changedOne(q.Exec(`INSERT INTO attachment_uploads(id,owner_uid,auth_epoch,origin_session,intent_key,room,room_epoch,recipient_uid,filename,mime,size,sha256,caption,day,expires_at,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'uploading')`, id, actor.UID, actor.AuthEpoch, session, in.IntentKey, scope, r, p, in.Filename, in.MIME, in.Size, in.SHA256, in.Caption, day, expires.Unix())); err != nil {
		return guiAttachmentReceipt{}, err
	}
	u, err := attachmentUploadQ(q, actor.UID, id, "")
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	if !attachmentIntentMatches(u, in, scope) || u.epoch != actor.AuthEpoch || u.originSession != session || u.roomEpoch != r || u.recipientUID != p || u.state != "uploading" || u.accepted != 0 || u.expires != expires.Unix() || u.day != day {
		return guiAttachmentReceipt{}, attachmentErr("storage_mismatch")
	}
	return attachmentReceiptQ(q, u, in.Op)
}

func attachmentChunkQ(q queryer, u *attachmentUpload, session string, in guiAttachmentRequest) error {
	if u.state != "uploading" {
		return attachmentErr("upload_not_active")
	}
	if session != u.originSession {
		return attachmentErr("origin_session_changed")
	}
	if in.Offset < 0 || in.Offset >= u.info.Size || in.Offset%attachmentChunkLimit != 0 || len(in.DataB64) == 0 || len(in.DataB64) > base64.StdEncoding.EncodedLen(attachmentChunkLimit) {
		return attachmentErr("chunk_invalid")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(in.DataB64)
	if err != nil {
		return attachmentErr("chunk_invalid")
	}
	want := min(int64(attachmentChunkLimit), u.info.Size-in.Offset)
	if int64(len(data)) != want || base64.StdEncoding.EncodeToString(data) != in.DataB64 {
		return attachmentErr("chunk_invalid")
	}
	if in.Offset < u.accepted {
		var old []byte
		if err = q.QueryRow(`SELECT data FROM attachment_chunks WHERE upload_id=? AND offset=?`, u.info.ID, in.Offset).Scan(&old); err != nil {
			return err
		}
		if !bytes.Equal(old, data) {
			return attachmentErr("chunk_conflict")
		}
		return nil
	}
	if in.Offset != u.accepted {
		return attachmentErr("offset_mismatch")
	}
	if err = changedOne(q.Exec(`INSERT INTO attachment_chunks(upload_id,offset,data) VALUES(?,?,?)`, u.info.ID, in.Offset, data)); err != nil {
		return err
	}
	if err = attachmentQuotaChangeQ(q, u.ownerUID, u.day, want, -want); err != nil {
		return err
	}
	if err = changedOne(q.Exec(`UPDATE attachment_uploads SET accepted=accepted+? WHERE id=? AND state='uploading' AND accepted=?`, want, u.info.ID, u.accepted)); err != nil {
		return err
	}
	var actual int64
	var stored []byte
	if err = q.QueryRow(`SELECT u.accepted,c.data FROM attachment_uploads u JOIN attachment_chunks c ON c.upload_id=u.id AND c.offset=? WHERE u.id=?`, in.Offset, u.info.ID).Scan(&actual, &stored); err != nil {
		return err
	}
	if actual != u.accepted+want || !bytes.Equal(stored, data) {
		return attachmentErr("storage_mismatch")
	}
	u.accepted = actual
	return nil
}

func attachmentDataQ(q queryer, u attachmentUpload) ([]byte, error) {
	if u.accepted != u.info.Size || u.info.Size < 1 || u.info.Size > attachmentItemLimit {
		return nil, attachmentErr("upload_incomplete")
	}
	rows, err := q.Query(`SELECT offset,data FROM attachment_chunks WHERE upload_id=? ORDER BY offset`, u.info.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := make([]byte, 0, int(u.info.Size))
	for rows.Next() {
		var offset int64
		var chunk []byte
		if err = rows.Scan(&offset, &chunk); err != nil {
			return nil, err
		}
		if offset != int64(len(data)) || len(chunk) != int(min(int64(attachmentChunkLimit), u.info.Size-offset)) || int64(len(data))+int64(len(chunk)) > u.info.Size {
			return nil, attachmentErr("storage_mismatch")
		}
		data = append(data, chunk...)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != u.info.Size {
		return nil, attachmentErr("storage_mismatch")
	}
	return data, nil
}

// Decode outside the writer transaction; the final writer revalidates the
// complete immutable upload, session, credential epoch, destination and clock.
var attachmentDecodeSlots = make(chan struct{}, 2)

func (s *Store) attachmentValidate(ctx context.Context, actor User, session, scope, id string) (attachmentInfo, error) {
	select {
	case attachmentDecodeSlots <- struct{}{}:
	case <-ctx.Done():
		return attachmentInfo{}, ctx.Err()
	}
	defer func() { <-attachmentDecodeSlots }()
	tx, err := s.readDB().BeginTx(ctx, nil)
	if err != nil {
		return attachmentInfo{}, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	fresh, err := attachmentActorQ(q, actor, session)
	if err != nil {
		return attachmentInfo{}, err
	}
	u, err := attachmentUploadQ(q, fresh.UID, id, "")
	if err != nil {
		return attachmentInfo{}, err
	}
	if err = attachmentAuthorizeQ(q, fresh, session, scope, u, u.state == "uploading"); err != nil {
		return attachmentInfo{}, err
	}
	if u.state != "uploading" {
		return u.info, nil
	}
	if session != u.originSession {
		return attachmentInfo{}, attachmentErr("origin_session_changed")
	}
	if s.attachmentClock().Unix() >= u.expires {
		return u.info, nil
	}
	data, err := attachmentDataQ(q, u)
	if err != nil {
		return attachmentInfo{}, err
	}
	if err = tx.Rollback(); err != nil {
		return attachmentInfo{}, err
	}
	info, err := validateAttachmentContent(data, u.info)
	if err != nil {
		return attachmentInfo{}, attachmentErr("content_invalid")
	}
	if err = ctx.Err(); err != nil {
		return attachmentInfo{}, err
	}
	return info, nil
}

func attachmentCommitQ(q queryer, actor User, session string, u *attachmentUpload, verified attachmentInfo, now time.Time) error {
	if u.state == "committed" {
		return nil
	}
	if u.state != "uploading" {
		return attachmentErr("upload_not_active")
	}
	if session != u.originSession {
		return attachmentErr("origin_session_changed")
	}
	if u.accepted != u.info.Size || verified.ID != u.info.ID || verified.SHA256 != u.info.SHA256 || verified.Size != u.info.Size || verified.MIME != u.info.MIME || verified.Filename != u.info.Filename {
		return attachmentErr("upload_incomplete")
	}
	room, recipient := u.room, ""
	if u.recipientUID > 0 {
		room = ""
		if err := q.QueryRow(`SELECT name FROM user_meta WHERE uid=?`, u.recipientUID).Scan(&recipient); err != nil {
			return attachmentErr("target_unavailable")
		}
	}
	if err := canSendQ(q, actor.Name, room, recipient); err != nil {
		return attachmentErr("target_denied")
	}
	body := "[附件] " + u.info.Filename
	if u.caption != "" {
		body = u.caption + "\n" + body
	}
	result, err := q.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,?,?,'attachment',?)`, room, actor.Name, recipient, body, now.Unix())
	if err = changedOne(result, err); err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err = changedOne(q.Exec(`INSERT INTO message_meta(id) VALUES(?)`, id)); err != nil {
		return err
	}
	if err = changedOne(q.Exec(`INSERT INTO message_attachments(message_id,upload_id,sender_uid,recipient_uid,room_epoch) VALUES(?,?,?,?,?)`, id, u.info.ID, actor.UID, u.recipientUID, u.roomEpoch)); err != nil {
		return err
	}
	if err = changedOne(q.Exec(`UPDATE attachment_uploads SET state='committed',message_id=?,width=?,height=?,frames=? WHERE id=? AND state='uploading' AND accepted=size`, id, verified.Width, verified.Height, verified.Frames, u.info.ID)); err != nil {
		return err
	}
	var count int
	if err = q.QueryRow(`SELECT count(*) FROM attachment_uploads u JOIN message_attachments a ON a.upload_id=u.id AND a.message_id=u.message_id JOIN messages m ON m.id=u.message_id WHERE u.id=? AND u.state='committed' AND u.message_id=? AND u.width=? AND u.height=? AND u.frames=? AND m.room=? AND m.sender=? AND m.recipient=? AND m.body=? AND m.kind='attachment' AND a.sender_uid=? AND a.recipient_uid=? AND a.room_epoch=?`, u.info.ID, id, verified.Width, verified.Height, verified.Frames, room, actor.Name, recipient, body, actor.UID, u.recipientUID, u.roomEpoch).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return attachmentErr("storage_mismatch")
	}
	if recipient != "" {
		if err = notifyQ(q, recipient, "dm", actor.Name, id, "收到附件"); err != nil {
			return err
		}
		if err = q.QueryRow(`SELECT count(*) FROM notifications WHERE user=? AND kind='dm' AND actor=? AND ref=?`, recipient, actor.Name, id).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return attachmentErr("storage_mismatch")
		}
	}
	u.state, u.messageID, u.info = "committed", id, verified
	return nil
}

func (s *Store) AttachmentForSession(ctx context.Context, actor User, session, scope string, in guiAttachmentRequest) (guiAttachmentReceipt, error) {
	out := guiAttachmentReceipt{Op: in.Op, Room: scope, IntentKey: in.IntentKey, UploadID: in.UploadID}
	if ctx == nil {
		return out, errCredentialsChanged
	}
	if !attachmentRequestValid(in) {
		return out, attachmentErr("request_invalid")
	}
	s.sweepExpiredAttachments(ctx)
	if in.Op == "fetch" {
		return s.attachmentFetch(ctx, actor, session, scope, in)
	}
	var verified attachmentInfo
	var validationErr error
	if in.Op == "commit" {
		verified, validationErr = s.attachmentValidate(ctx, actor, session, scope, in.UploadID)
		var typed attachmentError
		if validationErr != nil && (!errors.As(validationErr, &typed) || typed.code != "content_invalid") {
			return out, validationErr
		}
	}
	err := s.attachmentWrite(ctx, actor, session, func(q queryer, fresh User, now time.Time) error {
		if err := attachmentExpireOwnerQ(q, fresh.UID, now); err != nil {
			return err
		}
		if in.Op == "limits" {
			day, _ := attachmentDay(now)
			quota, err := attachmentQuotaQ(q, fresh.UID, day)
			if err != nil {
				return err
			}
			out.Status = "limits"
			out.OwnerUID = fresh.UID
			out.Quota = &quota
			out.ChunkLimit = attachmentChunkLimit
			out.AllowedMIMEs = append([]string(nil), attachmentMIMEs...)
			return nil
		}
		if in.Op == "intent" {
			var err error
			out, err = attachmentIntentQ(q, fresh, session, scope, in, now)
			return err
		}
		if in.Op == "reference" {
			var err error
			out, err = attachmentReferenceQ(q, fresh, session, scope, in, now)
			return err
		}
		if in.Op == "status" && in.IntentKey != "" {
			if ref, err := attachmentReferenceIntentQ(q, fresh.UID, in.IntentKey); err == nil {
				out, err = attachmentReferenceReceiptQ(q, fresh, scope, in.Op, ref, now)
				return err
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		u, err := attachmentUploadQ(q, fresh.UID, in.UploadID, in.IntentKey)
		if errors.Is(err, sql.ErrNoRows) && in.Op == "status" {
			out.Status = "unknown"
			out.Code = "not_found"
			return nil
		}
		if err != nil {
			return attachmentErr("upload_unavailable")
		}
		write := in.Op == "chunk" || in.Op == "commit"
		if u.state == "committed" {
			write = false
		}
		if err = attachmentAuthorizeQ(q, fresh, session, scope, u, write); err != nil {
			return err
		}
		newCommit := false
		switch in.Op {
		case "status":
		case "cancel":
			if err = attachmentFinishStateQ(q, &u, "cancelled"); err != nil {
				return err
			}
		case "chunk":
			if u.state == "uploading" {
				if err = attachmentChunkQ(q, &u, session, in); err != nil {
					return err
				}
			}
		case "commit":
			if u.state == "expired" || u.state == "cancelled" || u.state == "rejected" {
				break
			}
			if validationErr != nil {
				if err = attachmentFinishStateQ(q, &u, "rejected"); err != nil {
					return err
				}
				out.Code = "content_invalid"
				break
			}
			newCommit = u.state != "committed"
			if err = attachmentCommitQ(q, fresh, session, &u, verified, now); err != nil {
				return err
			}
		default:
			return attachmentErr("operation_invalid")
		}
		code := out.Code
		out, err = attachmentReceiptQ(q, u, in.Op)
		out.NewCommit = newCommit
		out.Code = code
		return err
	})
	if err != nil {
		var unknown attachmentUnknownError
		if errors.As(err, &unknown) {
			out = guiAttachmentReceipt{Op: in.Op, Status: "unknown", Code: "commit_unknown", Room: scope, IntentKey: in.IntentKey, UploadID: in.UploadID}
			if in.Op == "reference" {
				out.SourceMessageID = in.MessageID
			}
			return out, nil
		}
	}
	return out, err
}

// String form is safe for diagnostics: metadata only, never bytes or paths.
func (u attachmentUpload) String() string {
	return fmt.Sprintf("attachment %s %s %d/%d", u.info.ID, u.state, u.accepted, u.info.Size)
}
