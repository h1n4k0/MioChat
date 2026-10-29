package main

import (
	"database/sql"
	"errors"
	"time"
)

type attachmentReferenceIntent struct {
	ownerUID, epoch, sourceID, size, roomEpoch, recipientUID, messageID, created int64
	key, originSession, uploadID, room, state, code                              string
}

func attachmentReferenceIntentQ(q queryer, uid int64, key string) (attachmentReferenceIntent, error) {
	var r attachmentReferenceIntent
	err := q.QueryRow(`SELECT owner_uid,intent_key,auth_epoch,origin_session,source_message_id,upload_id,size,room,room_epoch,recipient_uid,COALESCE(message_id,0),created_at,state,code FROM attachment_reference_intents WHERE owner_uid=? AND intent_key=?`, uid, key).Scan(&r.ownerUID, &r.key, &r.epoch, &r.originSession, &r.sourceID, &r.uploadID, &r.size, &r.room, &r.roomEpoch, &r.recipientUID, &r.messageID, &r.created, &r.state, &r.code)
	return r, err
}

// Authenticated, new-intent denials are durable outcomes too: a lost rejection
// ACK can be resolved by the same key, rather than trapping the UI in UNKNOWN.
// Only caller-supplied source/target descriptors are stored, no denied metadata.
func attachmentRejectReferenceQ(q queryer, actor User, session, scope string, in guiAttachmentRequest, code string, now time.Time) (guiAttachmentReceipt, error) {
	if err := changedOne(q.Exec(`INSERT INTO attachment_reference_intents(owner_uid,intent_key,auth_epoch,origin_session,source_message_id,upload_id,size,room,room_epoch,recipient_uid,message_id,created_at,state,code) VALUES(?,?,?,?,?,'',0,?,0,0,NULL,?,'rejected',?)`, actor.UID, in.IntentKey, actor.AuthEpoch, session, in.MessageID, scope, now.Unix(), code)); err != nil {
		return guiAttachmentReceipt{}, err
	}
	r, err := attachmentReferenceIntentQ(q, actor.UID, in.IntentKey)
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	if r.state != "rejected" || r.code != code || r.originSession != session || r.epoch != actor.AuthEpoch || r.sourceID != in.MessageID || r.room != scope || r.messageID != 0 || r.size != 0 || r.uploadID != "" || r.created != now.Unix() {
		return guiAttachmentReceipt{}, attachmentErr("storage_mismatch")
	}
	return attachmentReferenceReceiptQ(q, actor, scope, in.Op, r, now)
}

func attachmentReferenceReceiptQ(q queryer, actor User, scope, op string, r attachmentReferenceIntent, now time.Time) (guiAttachmentReceipt, error) {
	out := guiAttachmentReceipt{Op: op, Status: "committed", IntentKey: r.key, UploadID: r.uploadID, Room: r.room, OwnerUID: r.ownerUID, Size: r.size, SourceMessageID: r.sourceID, MessageID: r.messageID, AcceptedOffset: 0}
	if actor.UID != r.ownerUID || actor.AuthEpoch != r.epoch {
		return out, errCredentialsChanged
	}
	if scope != r.room {
		return out, attachmentErr("target_changed")
	}
	if r.state == "rejected" {
		out.Status = "rejected"
		out.Code = r.code
		return out, nil
	}
	re, peer, err := attachmentTargetQ(q, actor, scope, false, false)
	if err != nil {
		return out, err
	}
	if re != r.roomEpoch || peer != r.recipientUID {
		return out, attachmentErr("target_changed")
	}
	day, _ := attachmentDay(now)
	quota, err := attachmentQuotaQ(q, actor.UID, day)
	if err != nil {
		return out, err
	}
	out.Quota = &quota
	blob, _, _, err := attachmentVisibleReferenceQ(q, actor, r.messageID)
	if err != nil {
		// The outcome remains committed even if subsequently recalled/deleted. Never
		// call this rolled back or invite a fresh intent. No stale file metadata.
		out.Code = "reference_unavailable"
		return out, nil
	}
	if blob.info.ID != r.uploadID || blob.info.Size != r.size {
		return out, attachmentErr("storage_mismatch")
	}
	info := blob.info
	out.Attachment = &info
	return out, nil
}

// Explicit, authorized reference publication: no bytes are received, no source
// blob is duplicated, and no quota used/reserved value is increased. Publication
// and the original intent outcome are durable in the same writer transaction.
func attachmentReferenceQ(q queryer, actor User, session, scope string, in guiAttachmentRequest, now time.Time) (guiAttachmentReceipt, error) {
	if !attachmentKeyValid(in.IntentKey) || in.MessageID <= 0 {
		return guiAttachmentReceipt{}, attachmentErr("reference_invalid")
	}
	if old, err := attachmentReferenceIntentQ(q, actor.UID, in.IntentKey); err == nil {
		if old.sourceID != in.MessageID || old.room != scope {
			return guiAttachmentReceipt{}, attachmentErr("intent_conflict")
		}
		return attachmentReferenceReceiptQ(q, actor, scope, in.Op, old, now)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return guiAttachmentReceipt{}, err
	}
	if _, err := attachmentUploadQ(q, actor.UID, "", in.IntentKey); err == nil {
		return guiAttachmentReceipt{}, attachmentErr("intent_conflict")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return guiAttachmentReceipt{}, err
	}
	blob, _, _, err := attachmentVisibleReferenceQ(q, actor, in.MessageID)
	if err != nil {
		return attachmentRejectReferenceQ(q, actor, session, scope, in, "source_unavailable", now)
	}
	roomEpoch, recipientUID, err := attachmentTargetQ(q, actor, scope, true, true)
	if err != nil {
		return attachmentRejectReferenceQ(q, actor, session, scope, in, "target_denied", now)
	}
	room, recipient := scope, ""
	if recipientUID > 0 {
		room = ""
		if err = q.QueryRow(`SELECT name FROM user_meta WHERE uid=?`, recipientUID).Scan(&recipient); err != nil {
			return guiAttachmentReceipt{}, attachmentErr("target_unavailable")
		}
	}
	body := "[附件] " + blob.info.Filename
	result, err := q.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,?,?,'attachment',?)`, room, actor.Name, recipient, body, now.Unix())
	if err = changedOne(result, err); err != nil {
		return guiAttachmentReceipt{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	if err = changedOne(q.Exec(`INSERT INTO message_meta(id) VALUES(?)`, id)); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if err = changedOne(q.Exec(`INSERT INTO message_attachments(message_id,upload_id,sender_uid,recipient_uid,room_epoch) VALUES(?,?,?,?,?)`, id, blob.info.ID, actor.UID, recipientUID, roomEpoch)); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if err = changedOne(q.Exec(`INSERT INTO attachment_reference_intents(owner_uid,intent_key,auth_epoch,origin_session,source_message_id,upload_id,size,room,room_epoch,recipient_uid,message_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, actor.UID, in.IntentKey, actor.AuthEpoch, session, in.MessageID, blob.info.ID, blob.info.Size, scope, roomEpoch, recipientUID, id, now.Unix())); err != nil {
		return guiAttachmentReceipt{}, err
	}
	stored, err := attachmentReferenceIntentQ(q, actor.UID, in.IntentKey)
	if err != nil {
		return guiAttachmentReceipt{}, err
	}
	if stored.epoch != actor.AuthEpoch || stored.originSession != session || stored.sourceID != in.MessageID || stored.uploadID != blob.info.ID || stored.size != blob.info.Size || stored.room != scope || stored.roomEpoch != roomEpoch || stored.recipientUID != recipientUID || stored.messageID != id || stored.created != now.Unix() {
		return guiAttachmentReceipt{}, attachmentErr("storage_mismatch")
	}
	var count int
	if err = q.QueryRow(`SELECT count(*) FROM message_attachments a JOIN messages m ON m.id=a.message_id JOIN message_meta x ON x.id=m.id WHERE a.message_id=? AND a.upload_id=? AND a.sender_uid=? AND a.recipient_uid=? AND a.room_epoch=? AND m.room=? AND m.sender=? AND m.recipient=? AND m.body=? AND m.kind='attachment' AND m.at=? AND x.recalled=0`, id, blob.info.ID, actor.UID, recipientUID, roomEpoch, room, actor.Name, recipient, body, now.Unix()).Scan(&count); err != nil {
		return guiAttachmentReceipt{}, err
	}
	if count != 1 {
		return guiAttachmentReceipt{}, attachmentErr("storage_mismatch")
	}
	if recipient != "" {
		if err = notifyQ(q, recipient, "dm", actor.Name, id, "收到附件"); err != nil {
			return guiAttachmentReceipt{}, err
		}
		if err = q.QueryRow(`SELECT count(*) FROM notifications WHERE user=? AND kind='dm' AND actor=? AND ref=?`, recipient, actor.Name, id).Scan(&count); err != nil {
			return guiAttachmentReceipt{}, err
		}
		if count != 1 {
			return guiAttachmentReceipt{}, attachmentErr("storage_mismatch")
		}
	}
	out, err := attachmentReferenceReceiptQ(q, actor, scope, in.Op, stored, now)
	if err != nil {
		return out, err
	}
	if out.Attachment == nil || out.Code != "" {
		return out, attachmentErr("storage_mismatch")
	}
	out.NewCommit = true
	return out, nil
}
