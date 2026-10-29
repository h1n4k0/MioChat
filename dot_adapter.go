package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// These methods are an internal product port, NOT HTTP handlers. No network
// listener accepts dotAdapterIdentity. The authenticated IPC/mTLS bridge and
// explicit OAuth link provisioning are intentionally not configured here.
func (s *Store) dotAdapterProfile(ctx context.Context, identity dotAdapterIdentity) (map[string]string, error) {
	var out map[string]string
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, profile, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		out = map[string]string{"id": profile, "name": u.Name}
		return nil
	})
	return out, err
}
func (s *Store) dotAdapterBindings(ctx context.Context, identity dotAdapterIdentity) ([]dotBindingView, error) {
	out := make([]dotBindingView, 0)
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		rows, e := q.Query(`SELECT ref FROM dot_bindings WHERE owner_uid=? ORDER BY created,ref LIMIT 100`, u.UID)
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
			b, e := dotBindingQ(q, ref)
			if e != nil {
				return e
			}
			if canReadQ(q, u.Name, b.Room) {
				out = append(out, b.dotBindingView)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) dotPrepareSubscription(ctx context.Context, identity dotAdapterIdentity, ref string) (dotBindingView, error) {
	var out dotBindingView
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ref)
		if e != nil || dotOwned(b, u) != nil {
			return errDotUnavailable
		}
		if e = dotLiveQ(q, b, b.Status == "ACTIVE"); e != nil {
			return e
		}
		if b.Status != "AWAITING_SUBSCRIPTION" && b.Status != "ACTIVE" {
			return errDotUnavailable
		}
		out = b.dotBindingView
		return nil
	})
	return out, err
}
func (s *Store) dotSubscribe(ctx context.Context, identity dotAdapterIdentity, in dotSubscriptionInput) error {
	if in.ID == "" || len(in.ID) > 128 || len(in.CallbackHash) != 64 || in.DeliveryHandle == "" || len(in.DeliveryHandle) > 256 {
		return errDotUnavailable
	}
	return s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, in.BindingRef)
		if e != nil || dotOwned(b, u) != nil || b.Generation != in.Generation {
			return errDotUnavailable
		}
		if e = dotLiveQ(q, b, b.Status == "ACTIVE"); e != nil {
			return e
		}
		if b.Status != "ACTIVE" && b.Status != "AWAITING_SUBSCRIPTION" {
			return errDotUnavailable
		}
		now := time.Now().Unix()
		if in.Expires <= now || in.Expires > min(identity.Expires, now+dotEventTTL) {
			return errDotUnavailable
		}
		var oldID, oldHash string
		var active int
		e = q.QueryRow(`SELECT id,callback_hash,active FROM dot_subscriptions WHERE binding_ref=?`, b.Ref).Scan(&oldID, &oldHash, &active)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil && active == 1 && (oldID != in.ID || oldHash != in.CallbackHash) {
			return errDotUnavailable
		}
		if e == nil {
			e = dotOne(q.Exec(`UPDATE dot_subscriptions SET id=?,generation=?,callback_hash=?,delivery_handle=?,expires=?,active=1,issuer=?,subject=?,token_issued_at=? WHERE binding_ref=?`, in.ID, b.Generation, in.CallbackHash, in.DeliveryHandle, in.Expires, identity.Issuer, identity.Subject, identity.IssuedAt, b.Ref))
		} else {
			e = dotOne(q.Exec(`INSERT INTO dot_subscriptions(id,binding_ref,generation,callback_hash,delivery_handle,expires,active,issuer,subject,token_issued_at) VALUES(?,?,?,?,?,?,1,?,?,?)`, in.ID, b.Ref, b.Generation, in.CallbackHash, in.DeliveryHandle, in.Expires, identity.Issuer, identity.Subject, identity.IssuedAt))
		}
		if e != nil {
			return e
		}
		if e = dotOne(q.Exec(`UPDATE dot_bindings SET status='ACTIVE',reason='' WHERE ref=? AND generation=?`, b.Ref, b.Generation)); e != nil {
			return e
		}
		b.Status, b.Reason = "ACTIVE", ""
		if e = dotAuditQ(q, u.Name, "subscribe", b.Ref, b.Room); e != nil {
			return e
		}
		if e = dotAssert(q, `SELECT count(*) FROM dot_subscriptions WHERE id=? AND binding_ref=? AND generation=? AND callback_hash=? AND delivery_handle=? AND expires=? AND active=1 AND issuer=? AND subject=? AND token_issued_at=?`, in.ID, b.Ref, b.Generation, in.CallbackHash, in.DeliveryHandle, in.Expires, identity.Issuer, identity.Subject, identity.IssuedAt); e != nil {
			return e
		}
		if _, _, e = dotAdapterActorQ(q, identity); e != nil {
			return errDotStorage
		}
		return dotFinalLiveQ(q, b)
	})
}
func (s *Store) dotUnsubscribe(ctx context.Context, identity dotAdapterIdentity, ref, subscriptionID, callbackHash string) error {
	return s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ref)
		if e != nil || dotOwned(b, u) != nil {
			return errDotUnavailable
		}
		var id, hash string
		var active int
		e = q.QueryRow(`SELECT id,callback_hash,active FROM dot_subscriptions WHERE binding_ref=?`, ref).Scan(&id, &hash, &active)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if id != subscriptionID || hash != callbackHash {
			return errDotUnavailable
		}
		if active == 0 {
			return nil
		}
		return dotInvalidateQ(q, b, "UNSUBSCRIBED")
	})
}

func dotIngestSourceQ(q queryer, bindingRef string, sourceID int64) (string, error) {
	var eventRef string
	err := func() error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		b, e := dotBindingQ(q, bindingRef)
		if e != nil {
			return e
		}
		if e = dotLiveQ(q, b, true); e != nil {
			return e
		}
		m, hash, e := dotSourceQ(q, sourceID)
		if e != nil {
			return e
		}
		now := time.Now().Unix()
		mention := false
		for _, name := range mentionTargets(m.Body) {
			if name == "dot" {
				mention = true
			}
		}
		if m.ID <= b.Watermark || m.Room != b.Room || m.Recipient != "" || m.Sender != b.OwnerName || m.Kind != "chat" || m.Recalled != 0 || !mention || m.At > now || m.At+dotEventTTL <= now {
			return nil
		}
		var causal int
		if e = q.QueryRow(`SELECT count(*) FROM dot_message_origins WHERE message_id=?`, m.ID).Scan(&causal); e != nil {
			return e
		}
		if causal != 0 {
			return nil
		}
		e = q.QueryRow(`SELECT ref FROM dot_events WHERE binding_ref=? AND generation=? AND source_id=?`, b.Ref, b.Generation, m.ID).Scan(&eventRef)
		if e == nil {
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var recent, pending int
		if e = q.QueryRow(`SELECT count(*) FROM dot_events WHERE binding_ref=? AND created>?`, b.Ref, now-600).Scan(&recent); e != nil {
			return e
		}
		if e = q.QueryRow(`SELECT count(*) FROM dot_events WHERE binding_ref=? AND status IN ('PENDING','SENDING','DELIVERED','UNKNOWN') AND expires>?`, b.Ref, now).Scan(&pending); e != nil {
			return e
		}
		if recent >= 10 || pending >= 20 {
			if e = dotInvalidateQ(q, b, "RATE_LIMITED"); e != nil {
				return e
			}
			return &dotPolicyDenied{"触发额度已用完"}
		}
		ref, e := dotRef("event")
		if e != nil {
			return e
		}
		read, e := dotRef("read")
		if e != nil {
			return e
		}
		reply, e := dotRef("reply")
		if e != nil {
			return e
		}
		ev := dotEvent{Ref: ref, BindingRef: b.Ref, SourceHash: hash, ReadRef: read, ReplyRef: reply, Status: "PENDING", Generation: b.Generation, SourceID: m.ID, Created: m.At, Expires: m.At + dotEventTTL}
		if e = dotOne(q.Exec(`INSERT INTO dot_events(ref,binding_ref,generation,source_id,source_hash,created,expires,read_ref,reply_ref,status) VALUES(?,?,?,?,?,?,?,?,?,'PENDING')`, ev.Ref, ev.BindingRef, ev.Generation, ev.SourceID, ev.SourceHash, ev.Created, ev.Expires, ev.ReadRef, ev.ReplyRef)); e != nil {
			return e
		}
		if e = dotAuditQ(q, b.OwnerName, "event", ref, b.Room); e != nil {
			return e
		}
		if e = dotEventReadbackQ(q, ev); e != nil {
			return e
		}
		if e = dotFinalLiveQ(q, b); e != nil {
			return e
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return errDotStorage
		}
		eventRef = ref
		return nil
	}()
	return eventRef, err
}
func (s *Store) dotIngestSource(ctx context.Context, bindingRef string, sourceID int64) (string, error) {
	var ref string
	err := s.dotTransaction(ctx, func(q queryer) error { var e error; ref, e = dotIngestSourceQ(q, bindingRef, sourceID); return e })
	return ref, err
}

// Capture eligible sources inside the ORIGINAL human message writer transaction.
// No history scan, background replay or nested Store transaction is involved.
// A hard storage error rolls back the human message and outbox together. Explicit
// policy pauses (e.g. bounded rate quota) may commit with the human room message;
// the room's sent receipt never claims that dot accepted or answered it.
func dotCaptureSourceQ(q queryer, source Message) error {
	var enabled string
	if e := q.QueryRow(`SELECT value FROM config WHERE key='dot_enabled'`).Scan(&enabled); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		return e
	}
	if enabled != "on" {
		return nil
	}
	check := func() error {
		m, _, e := dotSourceQ(q, source.ID)
		if e != nil || m.ID != source.ID || m.Room != source.Room || m.Sender != source.Sender || m.Recipient != source.Recipient || m.Body != source.Body || m.Kind != source.Kind || m.At != source.At || m.ReplyTo != source.ReplyTo || m.Edited != 0 || m.Recalled != 0 {
			return errDotStorage
		}
		return nil
	}
	if e := check(); e != nil {
		return e
	}
	rows, e := q.Query(`SELECT ref FROM dot_bindings WHERE room=? AND owner_name=? AND status='ACTIVE'`, source.Room, source.Sender)
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
		_, e = dotIngestSourceQ(q, ref, source.ID)
		var denied *dotPolicyDenied
		if e != nil && !errors.As(e, &denied) {
			return e
		}
	}
	return check()
}

func dotBodyValid(body string) bool {
	return utf8.ValidString(body) && utf8.RuneCountInString(body) <= dotMaxBodyRunes && body != "" && body == cleanText(body, dotMaxBodyRunes)
}
func (s *Store) dotReadContext(ctx context.Context, identity dotAdapterIdentity, readRef string) (map[string]any, error) {
	var out map[string]any
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		var ref string
		if e = q.QueryRow(`SELECT ref FROM dot_events WHERE read_ref=?`, readRef).Scan(&ref); e != nil {
			return errDotUnavailable
		}
		ev, e := dotEventQ(q, ref)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ev.BindingRef)
		if e != nil || dotOwned(b, u) != nil {
			return errDotUnavailable
		}
		if e = dotLiveQ(q, b, true); e != nil {
			return e
		}
		if ev.Status != "DELIVERED" {
			return errDotUnavailable
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return e
		}
		m, _, e := dotSourceQ(q, ev.SourceID)
		if e != nil {
			return e
		}
		out = map[string]any{"reply_ref": ev.ReplyRef, "source_id": m.ID, "room": b.Room, "messages": []map[string]any{{"id": m.ID, "sender": m.Sender, "body": m.Body, "at": m.At}}, "context_scope": "source_message_only"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) dotProposeReply(ctx context.Context, identity dotAdapterIdentity, replyRef, body, idem string) (dotOperationView, error) {
	var out dotOperationView
	if !dotBodyValid(body) || idem == "" || len(idem) > 128 || strings.TrimSpace(idem) != idem {
		return out, errDotUnavailable
	}
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		var ref string
		if e = q.QueryRow(`SELECT ref FROM dot_events WHERE reply_ref=?`, replyRef).Scan(&ref); e != nil {
			return errDotUnavailable
		}
		ev, e := dotEventQ(q, ref)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ev.BindingRef)
		if e != nil || dotOwned(b, u) != nil {
			return errDotUnavailable
		}
		if e = dotLiveQ(q, b, true); e != nil {
			return e
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return e
		}
		var prior string
		e = q.QueryRow(`SELECT ref FROM dot_operations WHERE binding_ref=? AND idem=?`, b.Ref, idem).Scan(&prior)
		if e == nil {
			o, e := dotOperationQ(q, prior)
			if e != nil {
				return e
			}
			if o.EventRef != ev.Ref || o.Body != body || o.BodyHash != dotHash(body) {
				return errDotUnavailable
			}
			if o.Status == "COMMITTED" {
				if e = dotCommittedReadbackQ(q, b, o); e != nil {
					return e
				}
			}
			out = o.dotOperationView
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if ev.Status != "DELIVERED" {
			return errDotUnavailable
		}
		_, epoch, audience, _, _, e := dotAudienceQ(q, b.Room)
		if e != nil {
			return e
		}
		operation, e := dotRef("operation")
		if e != nil {
			return e
		}
		o := dotOperation{dotOperationView: dotOperationView{Ref: operation, BindingRef: b.Ref, Status: "AWAITING_CONFIRMATION", SourceID: ev.SourceID}, Generation: b.Generation, EventRef: ev.Ref, Idem: idem, Body: body, BodyHash: dotHash(body), SourceHash: ev.SourceHash, AudienceHash: audience, RoomEpoch: epoch, Created: time.Now().Unix(), Expires: ev.Expires}
		if e = dotOne(q.Exec(`INSERT INTO dot_operations(ref,binding_ref,generation,event_ref,idem,body,body_hash,source_id,source_hash,room_epoch,audience_hash,status,created,expires) VALUES(?,?,?,?,?,?,?,?,?,?,?,'AWAITING_CONFIRMATION',?,?)`, o.Ref, o.BindingRef, o.Generation, o.EventRef, o.Idem, o.Body, o.BodyHash, o.SourceID, o.SourceHash, o.RoomEpoch, o.AudienceHash, o.Created, o.Expires)); e != nil {
			return e
		}
		if e = dotAuditQ(q, u.Name, "propose", o.Ref, b.Room); e != nil {
			return e
		}
		if e = dotOperationReadbackQ(q, o); e != nil {
			return e
		}
		if e = dotFinalLiveQ(q, b); e != nil {
			return e
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return errDotStorage
		}
		out = o.dotOperationView
		return nil
	})
	return out, err
}
func (s *Store) dotGetOperation(ctx context.Context, identity dotAdapterIdentity, ref string) (dotOperationView, error) {
	var out dotOperationView
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		o, e := dotOperationQ(q, ref)
		if e != nil {
			return errDotUnavailable
		}
		b, e := dotBindingQ(q, o.BindingRef)
		if e != nil || dotOwned(b, u) != nil || !canReadQ(q, u.Name, b.Room) {
			return errDotUnavailable
		}
		if o.Status == "COMMITTED" {
			if e = dotCommittedReadbackQ(q, b, o); e != nil {
				return e
			}
		}
		out = o.dotOperationView
		return nil
	})
	return out, err
}
