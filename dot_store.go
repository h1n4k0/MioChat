package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

func (s *Store) dotTransaction(ctx context.Context, fn func(queryer) error) error {
	if ctx == nil {
		return errDotUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	err = fn(q)
	var denied *dotPolicyDenied
	if err != nil && !errors.As(err, &denied) {
		return err
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := tx.Commit(); e != nil {
		return fmt.Errorf("dot 提交状态未知；只查询原操作状态，不得重发: %w", e)
	}
	return err
}
func dotFeatureQ(q queryer) error {
	var value string
	if e := q.QueryRow(`SELECT value FROM config WHERE key='dot_enabled'`).Scan(&value); e != nil {
		return e
	}
	if value != "on" {
		return errDotDisabled
	}
	return nil
}
func dotOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errDotStorage
	}
	return nil
}
func dotAssert(q queryer, statement string, args ...any) error {
	var n int
	if err := q.QueryRow(statement, args...).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return errDotStorage
	}
	return nil
}
func dotAuditQ(q queryer, actor, action, target, room string) error {
	now := time.Now().Unix()
	result, err := q.Exec(`INSERT INTO audit(actor,action,target,detail,room,at) VALUES(?,?,?,'',?,?)`, actor, "dot."+action, target, room, now)
	if err = dotOne(result, err); err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	return dotAssert(q, `SELECT count(*) FROM audit WHERE id=? AND actor=? AND action=? AND target=? AND detail='' AND room=? AND at=?`, id, actor, "dot."+action, target, room, now)
}
func dotOwnerQ(q queryer, user, sessionID string) (User, error) {
	if sessionID == "" {
		return User{}, errCredentialsChanged
	}
	if ok, e := sessionCurrentQ(q, sessionID, user); e != nil || !ok {
		return User{}, errCredentialsChanged
	}
	return mutationActorQ(q, user)
}
func dotAdapterActorQ(q queryer, identity dotAdapterIdentity) (User, string, error) {
	now := time.Now().Unix()
	if identity.Issuer == "" || identity.Subject == "" || identity.IssuedAt > now || identity.Expires <= now {
		return User{}, "", errDotUnavailable
	}
	var name, profile string
	var uid, epoch, after int64
	var active int
	err := q.QueryRow(`SELECT m.name,l.owner_uid,l.auth_epoch,l.profile_id,l.tokens_after,l.active FROM dot_oauth_links l JOIN user_meta m ON m.uid=l.owner_uid WHERE l.issuer=? AND l.subject=?`, identity.Issuer, identity.Subject).Scan(&name, &uid, &epoch, &profile, &after, &active)
	if err != nil {
		return User{}, "", errDotUnavailable
	}
	u, err := mutationActorQ(q, name)
	if err != nil || active != 1 || u.UID != uid || u.AuthEpoch != epoch || identity.IssuedAt < after || profile == "" {
		return User{}, "", errDotUnavailable
	}
	return u, profile, nil
}
func dotBindingQ(q queryer, ref string) (dotBinding, error) {
	var b dotBinding
	e := q.QueryRow(`SELECT ref,room,status,reason,generation,owner_uid,owner_name,auth_epoch,room_epoch,watermark,created FROM dot_bindings WHERE ref=?`, ref).Scan(&b.Ref, &b.Room, &b.Status, &b.Reason, &b.Generation, &b.OwnerUID, &b.OwnerName, &b.AuthEpoch, &b.RoomEpoch, &b.Watermark, &b.Created)
	return b, e
}
func dotOperationQ(q queryer, ref string) (dotOperation, error) {
	var o dotOperation
	e := q.QueryRow(`SELECT ref,binding_ref,status,source_id,message_id,generation,event_ref,idem,body,body_hash,source_hash,audience_hash,room_epoch,created,expires FROM dot_operations WHERE ref=?`, ref).Scan(&o.Ref, &o.BindingRef, &o.Status, &o.SourceID, &o.MessageID, &o.Generation, &o.EventRef, &o.Idem, &o.Body, &o.BodyHash, &o.SourceHash, &o.AudienceHash, &o.RoomEpoch, &o.Created, &o.Expires)
	return o, e
}
func dotEventQ(q queryer, ref string) (dotEvent, error) {
	var e dotEvent
	err := q.QueryRow(`SELECT ref,binding_ref,source_hash,read_ref,reply_ref,status,lease_id,generation,source_id,created,expires,attempts,next_attempt FROM dot_events WHERE ref=?`, ref).Scan(&e.Ref, &e.BindingRef, &e.SourceHash, &e.ReadRef, &e.ReplyRef, &e.Status, &e.LeaseID, &e.Generation, &e.SourceID, &e.Created, &e.Expires, &e.Attempts, &e.NextAttempt)
	return e, err
}
func dotOwned(b dotBinding, u User) error {
	if b.OwnerUID != u.UID || b.OwnerName != u.Name {
		return errDotUnavailable
	}
	return nil
}
func dotBindingReadbackQ(q queryer, want dotBinding) error {
	got, e := dotBindingQ(q, want.Ref)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(got, want) {
		return errDotStorage
	}
	return nil
}
func dotOperationReadbackQ(q queryer, want dotOperation) error {
	got, e := dotOperationQ(q, want.Ref)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(got, want) {
		return errDotStorage
	}
	return nil
}
func dotEventReadbackQ(q queryer, want dotEvent) error {
	got, e := dotEventQ(q, want.Ref)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(got, want) {
		return errDotStorage
	}
	return nil
}
func dotAudienceQ(q queryer, room string) (RoomInfo, int64, string, string, []int64, error) {
	r, err := roomQ(q, room)
	if err != nil {
		return r, 0, "", "", nil, err
	}
	var epoch int64
	if err = q.QueryRow(`SELECT epoch FROM dot_room_epochs WHERE room=?`, room).Scan(&epoch); err != nil || epoch < 1 {
		return r, 0, "", "", nil, errDotStorage
	}
	ids := make([]int64, 0)
	label := "本公开频道的全部有权访问账户（包括未来加入者）"
	if r.Private {
		label = "本私密频道当前有权者 UID 如下；作为频道历史，未来获得该频道权限者也可能读取"
		rows, e := q.Query(`SELECT DISTINCT m.uid FROM user_meta m WHERE m.name=? OR EXISTS(SELECT 1 FROM members a WHERE a.room=? AND a.user=m.name) ORDER BY m.uid`, r.Owner, room)
		if e != nil {
			return r, 0, "", "", nil, e
		}
		for rows.Next() {
			var id int64
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return r, 0, "", "", nil, e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return r, 0, "", "", nil, e
		}
	}
	raw, e := json.Marshal(struct {
		VisibilityPolicy            string
		Room, Owner                 string
		Private, Archived, ReadOnly bool
		Epoch                       int64
		UIDs                        []int64
	}{"room_history_current_and_future_authorized", r.Name, r.Owner, r.Private, r.Archived, r.ReadOnly, epoch, ids})
	if e != nil {
		return r, 0, "", "", nil, e
	}
	return r, epoch, dotHash(string(raw)), label, ids, nil
}
func dotInvalidateQ(q queryer, b dotBinding, reason string) error {
	if b.Generation >= 1<<63-1 {
		return errDotStorage
	}
	if e := dotOne(q.Exec(`UPDATE dot_bindings SET status='PAUSED',reason=?,generation=generation+1 WHERE ref=? AND generation=?`, reason, b.Ref, b.Generation)); e != nil {
		return e
	}
	b.Status, b.Reason, b.Generation = "PAUSED", reason, b.Generation+1
	// Enumerate mandatory transitions; no ignored multi-row writes are accepted.
	for _, spec := range []struct{ table, predicate, status string }{{"dot_events", "status NOT IN ('COMMITTED','CANCELLED','FAILED','PAUSED')", "PAUSED"}, {"dot_operations", "status='AWAITING_CONFIRMATION'", "CANCELLED"}} {
		rows, e := q.Query(`SELECT ref FROM `+spec.table+` WHERE binding_ref=? AND `+spec.predicate, b.Ref)
		if e != nil {
			return e
		}
		var refs []string
		for rows.Next() {
			var ref string
			if e = rows.Scan(&ref); e != nil {
				rows.Close()
				return e
			}
			refs = append(refs, ref)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, ref := range refs {
			if e = dotOne(q.Exec(`UPDATE `+spec.table+` SET status=? WHERE ref=?`, spec.status, ref)); e != nil {
				return e
			}
		}
		for _, ref := range refs {
			if e = dotAssert(q, `SELECT count(*) FROM `+spec.table+` WHERE ref=? AND status=?`, ref, spec.status); e != nil {
				return e
			}
		}
	}
	var id string
	e := q.QueryRow(`SELECT id FROM dot_subscriptions WHERE binding_ref=?`, b.Ref).Scan(&id)
	if e == nil {
		if e = dotOne(q.Exec(`UPDATE dot_subscriptions SET active=0 WHERE id=?`, id)); e != nil {
			return e
		}
		if e = dotAssert(q, `SELECT count(*) FROM dot_subscriptions WHERE id=? AND active=0`, id); e != nil {
			return e
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e = dotAuditQ(q, b.OwnerName, "pause", b.Ref, b.Room); e != nil {
		return e
	}
	if e = dotBindingReadbackQ(q, b); e != nil {
		return e
	}
	var pending int
	if e = q.QueryRow(`SELECT (SELECT count(*) FROM dot_events WHERE binding_ref=? AND status NOT IN ('COMMITTED','CANCELLED','FAILED','PAUSED'))+(SELECT count(*) FROM dot_operations WHERE binding_ref=? AND status='AWAITING_CONFIRMATION')`, b.Ref, b.Ref).Scan(&pending); e != nil {
		return e
	}
	if pending != 0 {
		return errDotStorage
	}
	return nil
}
func dotLiveQ(q queryer, b dotBinding, active bool) error {
	u, err := mutationActorQ(q, b.OwnerName)
	r, epoch, _, _, _, roomErr := dotAudienceQ(q, b.Room)
	if err != nil || roomErr != nil || u.UID != b.OwnerUID || u.AuthEpoch != b.AuthEpoch || r.Owner != b.OwnerName || !canReadRoomQ(q, u.Name, r) || r.Archived || epoch != b.RoomEpoch {
		if e := dotInvalidateQ(q, b, "AUTHORITY_CHANGED"); e != nil {
			return e
		}
		return &dotPolicyDenied{"权限或受众已变化；必须重新显式启用"}
	}
	if active {
		if b.Status != "ACTIVE" {
			return errDotUnavailable
		}
		var n int
		err = q.QueryRow(`SELECT count(*) FROM dot_subscriptions s JOIN dot_oauth_links l ON l.issuer=s.issuer AND l.subject=s.subject WHERE s.binding_ref=? AND s.generation=? AND s.active=1 AND s.expires>? AND l.active=1 AND l.owner_uid=? AND l.auth_epoch=? AND s.token_issued_at>=l.tokens_after`, b.Ref, b.Generation, time.Now().Unix(), b.OwnerUID, b.AuthEpoch).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			if e := dotInvalidateQ(q, b, "SUBSCRIPTION_EXPIRED"); e != nil {
				return e
			}
			return &dotPolicyDenied{"订阅过期"}
		}
	}
	return nil
}

// Final checks NEVER commit-on-policy-denial: a later trigger changing source or
// ACL must roll back all publication writes rather than commit a partial result.
func dotFinalLiveQ(q queryer, b dotBinding) error {
	if e := dotFeatureQ(q); e != nil {
		return errDotStorage
	}
	if e := dotBindingReadbackQ(q, b); e != nil {
		return e
	}
	u, e := mutationActorQ(q, b.OwnerName)
	if e != nil || u.UID != b.OwnerUID || u.AuthEpoch != b.AuthEpoch {
		return errDotStorage
	}
	r, epoch, _, _, _, e := dotAudienceQ(q, b.Room)
	if e != nil || r.Owner != u.Name || r.Archived || epoch != b.RoomEpoch || !canReadRoomQ(q, u.Name, r) {
		return errDotStorage
	}
	if b.Status == "ACTIVE" {
		if e = dotAssert(q, `SELECT count(*) FROM dot_subscriptions s JOIN dot_oauth_links l ON l.issuer=s.issuer AND l.subject=s.subject WHERE s.binding_ref=? AND s.generation=? AND s.active=1 AND s.expires>? AND l.active=1 AND l.owner_uid=? AND l.auth_epoch=? AND s.token_issued_at>=l.tokens_after`, b.Ref, b.Generation, time.Now().Unix(), b.OwnerUID, b.AuthEpoch); e != nil {
			return errDotStorage
		}
	}
	return nil
}
func dotSourceQ(q queryer, id int64) (Message, string, error) {
	var m Message
	var revision int64
	err := q.QueryRow(`SELECT m.id,m.room,m.sender,m.recipient,m.body,m.kind,m.at,COALESCE(x.reply_to,0),COALESCE(x.edited,0),COALESCE(x.recalled,0),COALESCE((SELECT max(r.id) FROM revisions r WHERE r.message_id=m.id),0) FROM messages m LEFT JOIN message_meta x ON x.id=m.id WHERE m.id=?`, id).Scan(&m.ID, &m.Room, &m.Sender, &m.Recipient, &m.Body, &m.Kind, &m.At, &m.ReplyTo, &m.Edited, &m.Recalled, &revision)
	if err != nil {
		return m, "", err
	}
	raw, err := json.Marshal(struct {
		Message  Message
		Revision int64
	}{m, revision})
	if err != nil {
		return m, "", err
	}
	return m, dotHash(string(raw)), nil
}
func dotCheckSourceQ(q queryer, b dotBinding, e dotEvent) error {
	m, hash, err := dotSourceQ(q, e.SourceID)
	if err != nil || hash != e.SourceHash || m.Sender != b.OwnerName || m.Room != b.Room || m.Recipient != "" || m.Kind != "chat" || m.Recalled != 0 || e.Generation != b.Generation || e.Expires <= time.Now().Unix() {
		return errDotUnavailable
	}
	var causal int
	if err = q.QueryRow(`SELECT count(*) FROM dot_message_origins WHERE message_id=?`, m.ID).Scan(&causal); err != nil {
		return err
	}
	if causal != 0 {
		return errDotUnavailable
	}
	return nil
}

func (s *Store) dotOwnerTransaction(ctx context.Context, user, sessionID string, fn func(queryer) error) error {
	return s.dotTransaction(ctx, func(q queryer) error {
		if err := fn(q); err != nil {
			return err
		}
		if _, err := dotOwnerQ(q, user, sessionID); err != nil {
			return errDotStorage
		}
		return nil
	})
}
