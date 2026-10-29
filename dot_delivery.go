package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) dotClaimDelivery(ctx context.Context, bindingRef string) (*dotDelivery, error) {
	var out *dotDelivery
	err := s.dotTransaction(ctx, func(q queryer) error {
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
		now := time.Now().Unix()
		var pending int
		if e = q.QueryRow(`SELECT count(*) FROM dot_events WHERE binding_ref=? AND status IN ('SENDING','DELIVERED','UNKNOWN') AND expires>?`, b.Ref, now).Scan(&pending); e != nil {
			return e
		}
		if pending != 0 {
			return nil
		}
		var ref string
		e = q.QueryRow(`SELECT ref FROM dot_events WHERE binding_ref=? AND status='PENDING' AND expires>? AND next_attempt<=? ORDER BY created,ref LIMIT 1`, b.Ref, now, now).Scan(&ref)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		ev, e := dotEventQ(q, ref)
		if e != nil {
			return e
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return e
		}
		var subID, handle string
		var expires int64
		if e = q.QueryRow(`SELECT id,delivery_handle,expires FROM dot_subscriptions WHERE binding_ref=? AND generation=? AND active=1`, b.Ref, b.Generation).Scan(&subID, &handle, &expires); e != nil {
			return e
		}
		if expires <= now {
			return errDotUnavailable
		}
		if ev.Attempts >= 3 {
			return errDotUnavailable
		}
		lease, e := dotRef("lease")
		if e != nil {
			return e
		}
		if e = dotOne(q.Exec(`UPDATE dot_events SET status='SENDING',attempts=attempts+1,lease_id=? WHERE ref=? AND status='PENDING'`, lease, ev.Ref)); e != nil {
			return e
		}
		ev.Status, ev.Attempts, ev.LeaseID = "SENDING", ev.Attempts+1, lease
		body, e := json.Marshal(map[string]any{"eventId": ev.Ref, "name": "mio.message.created.v1", "timestamp": time.Unix(ev.Created, 0).UTC().Format(time.RFC3339), "data": map[string]any{"schema_version": 1, "binding_ref": b.Ref, "event_ref": ev.Ref, "read_ref": ev.ReadRef}, "cursor": nil})
		if e != nil {
			return e
		}
		if e = dotAuditQ(q, b.OwnerName, "sending", ev.Ref, b.Room); e != nil {
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
		if e = dotAssert(q, `SELECT count(*) FROM dot_subscriptions WHERE id=? AND binding_ref=? AND generation=? AND active=1 AND delivery_handle=? AND expires=?`, subID, b.Ref, b.Generation, handle, expires); e != nil {
			return e
		}
		out = &dotDelivery{LeaseID: lease, EventID: ev.Ref, SubscriptionID: subID, DeliveryHandle: handle, Body: body}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) dotFinishDelivery(ctx context.Context, leaseID, outcome string) (map[string]string, error) {
	switch outcome {
	case "ACCEPTED", "TEMPORARY_FAILURE", "GONE", "TOO_LARGE", "PERMANENT_FAILURE", "UNKNOWN":
	default:
		return nil, errDotUnavailable
	}
	var out map[string]string
	err := s.dotTransaction(ctx, func(q queryer) error {
		// Even if disabled/revoked during transport, record what was observed without
		// reviving local access. No network side effect occurs in this transaction.
		var ref string
		if e := q.QueryRow(`SELECT ref FROM dot_events WHERE lease_id=?`, leaseID).Scan(&ref); e != nil {
			return errDotUnavailable
		}
		ev, e := dotEventQ(q, ref)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ev.BindingRef)
		if e != nil {
			return e
		}
		if dotFeatureQ(q) != nil && b.Status == "ACTIVE" {
			if e = dotInvalidateQ(q, b, "FEATURE_DISABLED"); e != nil {
				return e
			}
		} else {
			e = dotLiveQ(q, b, true)
			var policy *dotPolicyDenied
			if e != nil && !errors.As(e, &policy) && e != errDotUnavailable {
				return e
			}
		}
		ev, e = dotEventQ(q, ref)
		if e != nil {
			return e
		}
		if ev.Status != "SENDING" && !(ev.Status == "UNKNOWN" && outcome == "ACCEPTED") {
			if e = dotAuditQ(q, b.OwnerName, "delivery_observed."+outcome, ev.Ref, b.Room); e != nil {
				return e
			}
			if e = dotEventReadbackQ(q, ev); e != nil {
				return e
			}
			out = map[string]string{"event_ref": ev.Ref, "status": ev.Status, "delivery_outcome": outcome}
			return nil
		}
		status := "UNKNOWN"
		switch outcome {
		case "ACCEPTED":
			status = "DELIVERED"
		case "TEMPORARY_FAILURE":
			if ev.Attempts < 3 {
				status = "PENDING"
			} else {
				status = "FAILED"
			}
		case "GONE", "TOO_LARGE", "PERMANENT_FAILURE":
			status = "FAILED"
		}
		next := time.Now().Unix() + (1 << uint(ev.Attempts))
		if e = dotOne(q.Exec(`UPDATE dot_events SET status=?,next_attempt=? WHERE ref=? AND lease_id=?`, status, next, ev.Ref, leaseID)); e != nil {
			return e
		}
		ev.Status, ev.NextAttempt = status, next
		if e = dotAuditQ(q, b.OwnerName, "delivery."+outcome, ev.Ref, b.Room); e != nil {
			return e
		}
		if e = dotEventReadbackQ(q, ev); e != nil {
			return e
		}
		out = map[string]string{"event_ref": ev.Ref, "status": status, "delivery_outcome": outcome}
		return nil
	})
	return out, err
}
func (s *Store) dotRecoverInterrupted(ctx context.Context) error {
	// Safe under overlapping recovery: UNKNOWN never becomes PENDING. No alleged
	// host receipt API and no inference that an interrupted network request failed.
	return s.dotTransaction(ctx, func(q queryer) error {
		rows, e := q.Query(`SELECT ref FROM dot_events WHERE status='SENDING'`)
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
			if e = dotOne(q.Exec(`UPDATE dot_events SET status='UNKNOWN' WHERE ref=? AND status='SENDING'`, ref)); e != nil {
				return e
			}
		}
		for _, ref := range refs {
			if e = dotAssert(q, `SELECT count(*) FROM dot_events WHERE ref=? AND status='UNKNOWN'`, ref); e != nil {
				return e
			}
		}
		return nil
	})
}
