package main

import (
	"context"
	"encoding/base64"
	"strings"
	"time"
)

// Surrounding readers still enforce ordinary message/room visibility. Each
// reference has its own immutable endpoint/room identities, not the blob's
// original destination. Retiring the original publication revokes its copies.
// One viewer-name parameter; expects messages alias m.
const attachmentMessageIdentitySQL = `(m.kind!='attachment' OR EXISTS(SELECT 1 FROM message_attachments ai JOIN attachment_uploads au ON au.id=ai.upload_id AND au.state='committed' JOIN user_meta av ON av.name=? WHERE ai.message_id=m.id AND (m.id=au.message_id OR EXISTS(SELECT 1 FROM message_meta ao WHERE ao.id=au.message_id AND ao.recalled=0)) AND ((m.recipient!='' AND (av.uid=ai.sender_uid OR av.uid=ai.recipient_uid) AND EXISTS(SELECT 1 FROM user_meta su JOIN user_meta ru ON ru.name=m.recipient WHERE su.name=m.sender AND su.uid=ai.sender_uid AND ru.uid=ai.recipient_uid)) OR (m.recipient='' AND EXISTS(SELECT 1 FROM attachment_room_epochs ae WHERE ae.id=ai.room_epoch AND ae.name=m.room)))))`

func attachmentMessageIdentityQ(q queryer, user string, id int64) bool {
	var n int
	err := q.QueryRow(`SELECT count(*) FROM messages m WHERE m.id=? AND `+attachmentMessageIdentitySQL, id, user).Scan(&n)
	return err == nil && n == 1
}

type attachmentMessageBinding struct{ senderUID, recipientUID, roomEpoch int64 }

// Read a live, visible message reference without loading source bytes. Used by
// both fetch and explicit reference creation under the caller's transaction.
func attachmentVisibleReferenceQ(q queryer, viewer User, messageID int64) (attachmentUpload, Message, attachmentMessageBinding, error) {
	var binding attachmentMessageBinding
	var id string
	var owner int64
	var m Message
	err := q.QueryRow(`SELECT a.upload_id,u.owner_uid,a.sender_uid,a.recipient_uid,a.room_epoch FROM message_attachments a JOIN attachment_uploads u ON u.id=a.upload_id AND u.state='committed' JOIN message_meta x ON x.id=a.message_id AND x.recalled=0 JOIN message_meta original ON original.id=u.message_id AND original.recalled=0 WHERE a.message_id=?`, messageID).Scan(&id, &owner, &binding.senderUID, &binding.recipientUID, &binding.roomEpoch)
	if err != nil {
		return attachmentUpload{}, m, binding, attachmentErr("attachment_unavailable")
	}
	u, err := attachmentUploadQ(q, owner, id, "")
	if err != nil {
		return u, m, binding, err
	}
	m, err = messageQ(q, messageID)
	if err != nil {
		return u, m, binding, err
	}
	if m.Kind != "attachment" {
		return u, m, binding, attachmentErr("attachment_unavailable")
	}
	if m.Recipient != "" {
		if binding.recipientUID <= 0 || (viewer.UID != binding.senderUID && viewer.UID != binding.recipientUID) {
			return u, m, binding, attachmentErr("target_denied")
		}
		var senderUID, recipientUID int64
		if err = q.QueryRow(`SELECT s.uid,r.uid FROM user_meta s,user_meta r WHERE s.name=? AND r.name=?`, m.Sender, m.Recipient).Scan(&senderUID, &recipientUID); err != nil || senderUID != binding.senderUID || recipientUID != binding.recipientUID {
			return u, m, binding, attachmentErr("target_denied")
		}
	} else {
		r, p, e := attachmentTargetQ(q, viewer, m.Room, false, false)
		if e != nil || r != binding.roomEpoch || p != 0 || binding.recipientUID != 0 {
			return u, m, binding, attachmentErr("target_denied")
		}
	}
	return u, m, binding, nil
}

func (s *Store) attachmentFetch(ctx context.Context, actor User, session, scope string, in guiAttachmentRequest) (guiAttachmentReceipt, error) {
	out := guiAttachmentReceipt{Op: in.Op, Room: scope, MessageID: in.MessageID}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.readDB().BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	fresh, err := attachmentActorQ(q, actor, session)
	if err != nil {
		return out, err
	}
	u, m, _, err := attachmentVisibleReferenceQ(q, fresh, in.MessageID)
	if err != nil {
		return out, err
	}
	if guiScopeForMessage(m, fresh.Name).key() != scope {
		return out, attachmentErr("target_changed")
	}
	if in.Offset < 0 || in.Offset > u.info.Size || (in.Offset != u.info.Size && in.Offset%attachmentChunkLimit != 0) {
		return out, attachmentErr("offset_mismatch")
	}
	var data []byte
	if in.Offset < u.info.Size {
		if err = q.QueryRow(`SELECT data FROM attachment_chunks WHERE upload_id=? AND offset=?`, u.info.ID, in.Offset).Scan(&data); err != nil {
			return out, err
		}
		if int64(len(data)) != min(int64(attachmentChunkLimit), u.info.Size-in.Offset) {
			return out, attachmentErr("storage_mismatch")
		}
	}
	if err = tx.Rollback(); err != nil {
		return out, err
	}
	info := u.info
	out.Status = "fetched"
	out.UploadID = u.info.ID
	out.Attachment = &info
	out.Size = info.Size
	out.AcceptedOffset = u.accepted
	out.Offset = in.Offset
	out.DataB64 = base64.StdEncoding.EncodeToString(data)
	out.More = in.Offset+int64(len(data)) < u.info.Size
	return out, nil
}

// At most one bounded metadata query for actual attachment messages; ordinary
// text pages need no attachment read. Never source bytes or N queries per page.
func (s *Store) decorateAttachmentsContext(ctx context.Context, messages []Message, user string) error {
	if len(messages) == 0 {
		return nil
	}
	args := make([]any, 0, len(messages)+3)
	for i := range messages {
		messages[i].Attachment = nil
		if messages[i].Kind == "attachment" {
			args = append(args, messages[i].ID)
		}
	}
	if len(args) == 0 {
		return nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	queryArgs := append([]any{user}, args...)
	queryArgs = append(queryArgs, user, user)
	rows, err := s.readQueryContext(ctx, `SELECT a.message_id,u.id,u.filename,u.mime,u.size,u.sha256,u.width,u.height,u.frames FROM message_attachments a JOIN attachment_uploads u ON u.id=a.upload_id AND u.state='committed' JOIN messages m ON m.id=a.message_id JOIN message_meta x ON x.id=m.id JOIN message_meta original ON original.id=u.message_id AND original.recalled=0 JOIN user_meta viewer ON viewer.name=? WHERE m.id IN (`+marks+`) AND x.recalled=0 AND ((m.recipient!='' AND (viewer.uid=a.sender_uid OR viewer.uid=a.recipient_uid) AND EXISTS(SELECT 1 FROM user_meta su JOIN user_meta ru ON ru.name=m.recipient WHERE su.name=m.sender AND su.uid=a.sender_uid AND ru.uid=a.recipient_uid)) OR (m.recipient='' AND EXISTS(SELECT 1 FROM attachment_room_epochs e JOIN rooms r ON r.name=e.name LEFT JOIN room_meta rm ON rm.name=r.name WHERE e.id=a.room_epoch AND r.name=m.room AND (COALESCE(rm.private,0)=0 OR r.owner=? OR EXISTS(SELECT 1 FROM members mm WHERE mm.room=r.name AND mm.user=?)))))`, queryArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64]*attachmentInfo)
	for rows.Next() {
		var id int64
		var info attachmentInfo
		if err = rows.Scan(&id, &info.ID, &info.Filename, &info.MIME, &info.Size, &info.SHA256, &info.Width, &info.Height, &info.Frames); err != nil {
			return err
		}
		info.MetadataPolicy = "preserve_original"
		byID[id] = &info
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if messages[i].Kind != "attachment" {
			continue
		}
		messages[i].Attachment = byID[messages[i].ID]
		if messages[i].Attachment == nil && messages[i].Recalled == 0 {
			messages[i].Body = "[附件不可用]"
			messages[i].ReplyName = ""
			messages[i].ReplyBody = ""
		}
	}
	return nil
}
