package main

import (
	"context"
	"crypto/subtle"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

func dotOwnerInputValid(in guiDotRequest) bool {
	if in.Room == "" || len(in.Room) > 24 || strings.HasPrefix(in.Room, "dm:") || in.OwnerOrigin == "" || len(in.OwnerOrigin) > 128 || !utf8.ValidString(in.OwnerOrigin) || cleanText(in.OwnerOrigin, 128) != in.OwnerOrigin {
		return false
	}
	rest := in
	rest.Operation = ""
	rest.Room = ""
	rest.OwnerOrigin = ""
	rest.EditGeneration = 0
	switch in.Operation {
	case "list", "create":
	case "enable", "pause", "pending":
		if in.BindingRef == "" || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 1 {
			return false
		}
		rest.BindingRef = ""
		rest.ExpectedGeneration = nil
	case "review", "cancel", "status":
		if in.OperationRef == "" {
			return false
		}
		rest.OperationRef = ""
	case "confirm":
		if in.OperationRef == "" || in.ExpectedGeneration == nil || *in.ExpectedGeneration < 1 || in.ReviewToken == "" || len(in.ReviewToken) > 128 || len(in.BodySHA256) != 64 || len(in.AudienceSHA256) != 64 || in.SourceID < 1 || !in.Confirmed {
			return false
		}
		rest.OperationRef = ""
		rest.ExpectedGeneration = nil
		rest.ReviewToken = ""
		rest.BodySHA256 = ""
		rest.AudienceSHA256 = ""
		rest.SourceID = 0
		rest.Confirmed = false
	default:
		return false
	}
	return reflect.DeepEqual(rest, guiDotRequest{}) && len(in.BindingRef) <= 128 && len(in.OperationRef) <= 128
}

func (s *Store) dotOwnerAction(ctx context.Context, user, sessionID string, in guiDotRequest) (guiDotReceipt, *Message, error) {
	out := guiDotReceipt{Operation: in.Operation, OwnerOrigin: in.OwnerOrigin, EditGeneration: in.EditGeneration}
	if !dotOwnerInputValid(in) {
		return guiDotReceipt{}, nil, errDotUnavailable
	}
	var committed *Message
	err := s.dotOwnerTransaction(ctx, user, sessionID, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, e := dotOwnerQ(q, user, sessionID)
		if e != nil {
			return e
		}
		if in.Operation == "list" || in.Operation == "create" {
			r, epoch, _, _, _, e := dotAudienceQ(q, in.Room)
			if e != nil {
				return e
			}
			if r.Owner != u.Name || r.Archived || !canReadRoomQ(q, u.Name, r) {
				return errDotUnavailable
			}
			if in.Operation == "create" {
				ref, e := dotRef("binding")
				if e != nil {
					return e
				}
				b := dotBinding{dotBindingView: dotBindingView{Ref: ref, Room: in.Room, Status: "DISABLED", Generation: 1, OwnerUID: u.UID}, OwnerName: u.Name, AuthEpoch: u.AuthEpoch, RoomEpoch: epoch, Created: time.Now().Unix()}
				if e = dotOne(q.Exec(`INSERT INTO dot_bindings(ref,owner_uid,owner_name,room,status,generation,auth_epoch,room_epoch,watermark,created) VALUES(?,?,?,?,'DISABLED',1,?,?,0,?)`, ref, u.UID, u.Name, in.Room, u.AuthEpoch, epoch, b.Created)); e != nil {
					return e
				}
				if e = dotAuditQ(q, u.Name, "create", ref, in.Room); e != nil {
					return e
				}
				if e = dotBindingReadbackQ(q, b); e != nil {
					return e
				}
				out.Binding = &b.dotBindingView
				return nil
			}
			rows, e := q.Query(`SELECT ref FROM dot_bindings WHERE owner_uid=? AND room=? ORDER BY created,ref`, u.UID, in.Room)
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
			out.Bindings = make([]dotBindingView, 0, len(refs))
			for _, ref := range refs {
				b, e := dotBindingQ(q, ref)
				if e != nil {
					return e
				}
				out.Bindings = append(out.Bindings, b.dotBindingView)
			}
			return nil
		}
		if in.BindingRef != "" {
			b, e := dotBindingQ(q, in.BindingRef)
			if e != nil || dotOwned(b, u) != nil || b.Room != in.Room || b.Generation != *in.ExpectedGeneration {
				return errDotUnavailable
			}
			switch in.Operation {
			case "pause":
				if e = dotInvalidateQ(q, b, "OWNER_PAUSED"); e != nil {
					return e
				}
				b, e = dotBindingQ(q, b.Ref)
				if e != nil {
					return e
				}
				out.Binding = &b.dotBindingView
				return nil
			case "enable":
				if b.Status == "ACTIVE" || b.Status == "AWAITING_SUBSCRIPTION" || b.Generation >= 1<<63-1 {
					return errDotUnavailable
				}
				r, epoch, _, _, _, e := dotAudienceQ(q, b.Room)
				if e != nil {
					return e
				}
				if r.Owner != u.Name || r.Archived || !canReadRoomQ(q, u.Name, r) {
					return errDotUnavailable
				}
				var watermark int64
				if e = q.QueryRow(`SELECT COALESCE(max(id),0) FROM messages`).Scan(&watermark); e != nil {
					return e
				}
				if e = dotOne(q.Exec(`UPDATE dot_bindings SET status='AWAITING_SUBSCRIPTION',reason='',generation=generation+1,auth_epoch=?,room_epoch=?,watermark=? WHERE ref=? AND generation=?`, u.AuthEpoch, epoch, watermark, b.Ref, b.Generation)); e != nil {
					return e
				}
				b.Status, b.Reason, b.Generation, b.AuthEpoch, b.RoomEpoch, b.Watermark = "AWAITING_SUBSCRIPTION", "", b.Generation+1, u.AuthEpoch, epoch, watermark
				if e = dotAuditQ(q, u.Name, "enable", b.Ref, b.Room); e != nil {
					return e
				}
				if e = dotFinalLiveQ(q, b); e != nil {
					return e
				}
				out.Binding = &b.dotBindingView
				return nil
			case "pending":
				if e = dotLiveQ(q, b, true); e != nil {
					return e
				}
				rows, e := q.Query(`SELECT ref FROM dot_operations WHERE binding_ref=? AND status='AWAITING_CONFIRMATION' AND expires>? ORDER BY created,ref LIMIT 20`, b.Ref, time.Now().Unix())
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
				out.Pending = make([]dotOperationView, 0, len(refs))
				for _, ref := range refs {
					o, e := dotOperationQ(q, ref)
					if e != nil {
						return e
					}
					out.Pending = append(out.Pending, o.dotOperationView)
				}
				return nil
			}
		}
		o, e := dotOperationQ(q, in.OperationRef)
		if e != nil {
			return errDotUnavailable
		}
		b, e := dotBindingQ(q, o.BindingRef)
		if e != nil || dotOwned(b, u) != nil || b.Room != in.Room {
			return errDotUnavailable
		}
		if o.Status == "COMMITTED" {
			if e = dotCommittedReadbackQ(q, b, o); e != nil {
				return e
			}
			if in.Operation == "confirm" && (o.Generation != *in.ExpectedGeneration || o.BodyHash != in.BodySHA256 || o.AudienceHash != in.AudienceSHA256 || o.SourceID != in.SourceID) {
				return errDotUnavailable
			}
			out.Result = &o.dotOperationView
			return nil
		}
		if in.Operation == "status" {
			out.Result = &o.dotOperationView
			return nil
		}
		if in.Operation == "cancel" {
			if o.Status == "AWAITING_CONFIRMATION" {
				if e = dotOne(q.Exec(`UPDATE dot_operations SET status='CANCELLED' WHERE ref=? AND status='AWAITING_CONFIRMATION'`, o.Ref)); e != nil {
					return e
				}
				o.Status = "CANCELLED"
				if e = dotOne(q.Exec(`UPDATE dot_events SET status='CANCELLED' WHERE ref=?`, o.EventRef)); e != nil {
					return e
				}
				if e = dotAuditQ(q, u.Name, "cancel", o.Ref, b.Room); e != nil {
					return e
				}
				if e = dotOperationReadbackQ(q, o); e != nil {
					return e
				}
				if e = dotAssert(q, `SELECT count(*) FROM dot_events WHERE ref=? AND status='CANCELLED'`, o.EventRef); e != nil {
					return e
				}
			}
			out.Result = &o.dotOperationView
			return nil
		}
		if e = dotLiveQ(q, b, true); e != nil {
			return e
		}
		ev, e := dotEventQ(q, o.EventRef)
		if e != nil {
			return e
		}
		if !dotBodyValid(o.Body) || dotHash(o.Body) != o.BodyHash || o.Status != "AWAITING_CONFIRMATION" || o.Generation != b.Generation || o.Expires <= time.Now().Unix() || ev.Status != "DELIVERED" {
			return errDotUnavailable
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return e
		}
		_, epoch, audience, label, ids, e := dotAudienceQ(q, b.Room)
		if e != nil {
			return e
		}
		if epoch != o.RoomEpoch || audience != o.AudienceHash || o.SourceHash != ev.SourceHash {
			return errDotUnavailable
		}
		if in.Operation == "review" {
			token, e := dotRef("review")
			if e != nil {
				return e
			}
			expires := min(o.Expires, time.Now().Unix()+300)
			if e = dotOne(q.Exec(`INSERT INTO dot_reviews(operation_ref,session_id,token_hash,expires) VALUES(?,?,?,?) ON CONFLICT(operation_ref,session_id) DO UPDATE SET token_hash=excluded.token_hash,expires=excluded.expires`, o.Ref, sessionID, dotHash(token), expires)); e != nil {
				return e
			}
			if e = dotAuditQ(q, u.Name, "review", o.Ref, b.Room); e != nil {
				return e
			}
			if e = dotAssert(q, `SELECT count(*) FROM dot_reviews WHERE operation_ref=? AND session_id=? AND token_hash=? AND expires=?`, o.Ref, sessionID, dotHash(token), expires); e != nil {
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
			out.Review = &dotReviewView{dotOperationView: o.dotOperationView, Room: b.Room, Body: o.Body, BodySHA256: o.BodyHash, AudienceSHA256: audience, AudienceLabel: label, AudienceUIDs: ids, Generation: o.Generation, OwnerUID: b.OwnerUID, ReviewToken: token, Expires: expires}
			return nil
		}
		if in.Operation != "confirm" || o.Generation != *in.ExpectedGeneration || o.BodyHash != in.BodySHA256 || o.AudienceHash != in.AudienceSHA256 || o.SourceID != in.SourceID {
			return errDotUnavailable
		}
		var tokenHash string
		var reviewExpires int64
		if e = q.QueryRow(`SELECT token_hash,expires FROM dot_reviews WHERE operation_ref=? AND session_id=?`, o.Ref, sessionID).Scan(&tokenHash, &reviewExpires); e != nil {
			return errDotUnavailable
		}
		if reviewExpires <= time.Now().Unix() || subtle.ConstantTimeCompare([]byte(tokenHash), []byte(dotHash(in.ReviewToken))) != 1 {
			return errDotUnavailable
		}
		if e = canSendQ(q, u.Name, b.Room, ""); e != nil {
			return e
		}
		message, e := dotPublishQ(q, b, &o, ev, u.Name)
		if e != nil {
			return e
		}
		if _, e = dotOwnerQ(q, u.Name, sessionID); e != nil {
			return errDotStorage
		}
		if e = dotAssert(q, `SELECT count(*) FROM dot_reviews WHERE operation_ref=? AND session_id=? AND token_hash=? AND expires=?`, o.Ref, sessionID, tokenHash, reviewExpires); e != nil {
			return e
		}
		if e = dotFinalLiveQ(q, b); e != nil {
			return e
		}
		if e = dotCheckSourceQ(q, b, ev); e != nil {
			return errDotStorage
		}
		_, actualEpoch, actualAudience, _, _, e := dotAudienceQ(q, b.Room)
		if e != nil || actualEpoch != epoch || actualAudience != audience {
			return errDotStorage
		}
		if e = dotCommittedReadbackQ(q, b, o); e != nil {
			return e
		}
		committed = &message
		out.Result = &o.dotOperationView
		return nil
	})
	if err != nil {
		return guiDotReceipt{}, nil, err
	}
	return out, committed, nil
}

func dotPublishQ(q queryer, b dotBinding, o *dotOperation, ev dotEvent, actor string) (Message, error) {
	var before int64
	if e := q.QueryRow(`SELECT COALESCE(max(id),0) FROM messages`).Scan(&before); e != nil {
		return Message{}, e
	}
	m := Message{Room: b.Room, Sender: dotAssistantSender, Body: o.Body, Kind: "assistant", At: time.Now().Unix(), ReplyTo: o.SourceID}
	result, e := q.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,'',?,'assistant',?)`, m.Room, m.Sender, m.Body, m.At)
	if e = dotOne(result, e); e != nil {
		return m, e
	}
	m.ID, e = result.LastInsertId()
	if e != nil {
		return m, e
	}
	if e = dotOne(q.Exec(`INSERT INTO message_meta(id,reply_to) VALUES(?,?)`, m.ID, m.ReplyTo)); e != nil {
		return m, e
	}
	if e = dotOne(q.Exec(`INSERT INTO dot_message_origins(message_id,event_ref,operation_ref) VALUES(?,?,?)`, m.ID, ev.Ref, o.Ref)); e != nil {
		return m, e
	}
	if e = dotOne(q.Exec(`UPDATE dot_operations SET status='COMMITTED',message_id=? WHERE ref=? AND status='AWAITING_CONFIRMATION'`, m.ID, o.Ref)); e != nil {
		return m, e
	}
	o.Status, o.MessageID = "COMMITTED", m.ID
	if e = dotOne(q.Exec(`UPDATE dot_events SET status='COMMITTED' WHERE ref=? AND status='DELIVERED'`, ev.Ref)); e != nil {
		return m, e
	}
	// No @mention expansion: owner approved a room reply, not extra outbound actions.
	if e = dotAuditQ(q, actor, "confirm", o.Ref, b.Room); e != nil {
		return m, e
	}
	if e = dotOperationReadbackQ(q, *o); e != nil {
		return m, e
	}
	if e = dotAssert(q, `SELECT count(*) FROM messages m JOIN message_meta x ON x.id=m.id WHERE m.id=? AND m.room=? AND m.sender=? AND m.recipient='' AND m.body=? AND m.kind='assistant' AND m.at=? AND x.reply_to=? AND x.recalled=0 AND x.edited=0`, m.ID, m.Room, m.Sender, m.Body, m.At, m.ReplyTo); e != nil {
		return m, e
	}
	if e = dotAssert(q, `SELECT count(*) FROM messages WHERE id>?`, before); e != nil {
		return m, e
	}
	return m, dotCommittedReadbackQ(q, b, *o)
}
func dotCommittedReadbackQ(q queryer, b dotBinding, o dotOperation) error {
	if o.Status != "COMMITTED" || o.MessageID < 1 || dotHash(o.Body) != o.BodyHash {
		return errDotStorage
	}
	if e := dotOperationReadbackQ(q, o); e != nil {
		return e
	}
	return dotAssert(q, `SELECT count(*) FROM dot_message_origins a JOIN messages m ON m.id=a.message_id JOIN message_meta x ON x.id=m.id JOIN dot_events e ON e.ref=a.event_ref WHERE a.operation_ref=? AND a.event_ref=? AND m.id=? AND m.room=? AND m.sender=? AND m.recipient='' AND m.body=? AND m.kind='assistant' AND x.reply_to=? AND x.recalled=0 AND x.edited=0 AND e.status='COMMITTED' AND e.binding_ref=? AND e.source_id=?`, o.Ref, o.EventRef, o.MessageID, b.Room, dotAssistantSender, o.Body, o.SourceID, b.Ref, o.SourceID)
}

func guiDotRequestAction(ctx context.Context, store *Store, hub *Hub, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	if request.RequestID == "" || len(request.RequestID) > 128 || request.Dot == nil || request.Type != "dot" || scope.peer != "" || request.Room != scope.key() || request.Dot.Room != scope.room {
		return guiResponse{}, errDotUnavailable
	}
	copy := request
	copy.Type = ""
	copy.RequestID = ""
	copy.Room = ""
	copy.Dot = nil
	copy.ctx = nil
	copy.sessionID = ""
	copy.connectionIP = ""
	copy.communityRoom = ""
	if !reflect.DeepEqual(copy, guiRequest{}) {
		return guiResponse{}, errDotUnavailable
	}
	receipt, message, e := store.dotOwnerAction(ctx, user.Name, sessionID, *request.Dot)
	if e != nil {
		return guiResponse{}, e
	}
	if message != nil {
		hub.Broadcast(*message)
	}
	return guiResponse{Type: "dot", RequestID: request.RequestID, Room: scope.room, Dot: &receipt}, nil
}
