package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type dotFixtureState struct {
	s        *Store
	h        *Hub
	u        User
	session  string
	identity dotAdapterIdentity
	binding  dotBindingView
}

func newDotFixture(t *testing.T) *dotFixtureState {
	t.Helper()
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	if e := s.CreateRoomAccess("dot-room", "alice", true); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`UPDATE config SET value='on' WHERE key='dot_enabled'`); e != nil {
		t.Fatal(e)
	}
	identity := dotAdapterIdentity{Issuer: "https://synthetic.invalid/issuer", Subject: "synthetic-alice", IssuedAt: time.Now().Unix() - 1, Expires: time.Now().Unix() + 3600}
	if _, e := s.db.Exec(`INSERT INTO dot_oauth_links VALUES(?,?,?,?,?,?,1)`, identity.Issuer, identity.Subject, m.user.UID, m.user.AuthEpoch, "synthetic-profile-alice", 0); e != nil {
		t.Fatal(e)
	}
	f := &dotFixtureState{s: s, h: h, u: m.user, session: m.client.ID, identity: identity}
	out, _, e := s.dotOwnerAction(t.Context(), "alice", f.session, f.req("create"))
	if e != nil || out.Binding == nil {
		t.Fatal(out, e)
	}
	f.binding = *out.Binding
	return f
}
func (f *dotFixtureState) req(op string) guiDotRequest {
	return guiDotRequest{Operation: op, Room: "dot-room", OwnerOrigin: "synthetic-private-pane", EditGeneration: 1}
}
func (f *dotFixtureState) enable(t *testing.T) {
	t.Helper()
	r := f.req("enable")
	r.BindingRef = f.binding.Ref
	r.ExpectedGeneration = &f.binding.Generation
	out, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, r)
	if e != nil || out.Binding == nil {
		t.Fatal(out, e)
	}
	f.binding = *out.Binding
	if e = f.s.dotSubscribe(t.Context(), f.identity, dotSubscriptionInput{ID: "synthetic-sub", BindingRef: f.binding.Ref, CallbackHash: dotHash("https://receiver.invalid/callback"), DeliveryHandle: "synthetic-vault-handle", Generation: f.binding.Generation, Expires: time.Now().Unix() + 600}); e != nil {
		t.Fatal(e)
	}
	f.binding.Status = "ACTIVE"
}
func (f *dotFixtureState) draft(t *testing.T) (dotOperationView, guiDotRequest) {
	t.Helper()
	f.enable(t)
	m, e := f.s.Send(Message{Room: "dot-room", Sender: "alice", Kind: "chat", Body: "@dot synthetic test question"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.dotIngestSource(t.Context(), f.binding.Ref, m.ID); e != nil {
		t.Fatal(e)
	}
	lease, e := f.s.dotClaimDelivery(t.Context(), f.binding.Ref)
	if e != nil || lease == nil {
		t.Fatal(lease, e)
	}
	var payload struct {
		Data struct {
			ReadRef string `json:"read_ref"`
		}
	}
	if e = json.Unmarshal(lease.Body, &payload); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.dotFinishDelivery(t.Context(), lease.LeaseID, "ACCEPTED"); e != nil {
		t.Fatal(e)
	}
	ctx, e := f.s.dotReadContext(t.Context(), f.identity, payload.Data.ReadRef)
	if e != nil {
		t.Fatal(e)
	}
	op, e := f.s.dotProposeReply(t.Context(), f.identity, ctx["reply_ref"].(string), "Synthetic exact owner-reviewed answer.", "synthetic-idempotency")
	if e != nil {
		t.Fatal(e)
	}
	review := f.req("review")
	review.OperationRef = op.Ref
	out, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, review)
	if e != nil || out.Review == nil {
		t.Fatal(out, e)
	}
	if !strings.Contains(out.Review.AudienceLabel, "未来获得该频道权限者也可能读取") {
		t.Fatal("private room history audience not disclosed", out.Review.AudienceLabel)
	}
	r := f.req("confirm")
	r.OperationRef = op.Ref
	r.ExpectedGeneration = &out.Review.Generation
	r.BodySHA256 = out.Review.BodySHA256
	r.AudienceSHA256 = out.Review.AudienceSHA256
	r.SourceID = out.Review.SourceID
	r.ReviewToken = out.Review.ReviewToken
	r.Confirmed = true
	return op, r
}
func dotAssistantCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if e := s.db.QueryRow(`SELECT count(*) FROM messages WHERE kind='assistant'`).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestDotDefaultOffOwnerOnlyAndNoImplicitEnable(t *testing.T) {
	s, h := featureFixture(t)
	m := testModel(t, s, h, "alice")
	if e := s.CreateRoomAccess("dot-room", "alice", true); e != nil {
		t.Fatal(e)
	}
	request := guiDotRequest{Operation: "create", Room: "dot-room", OwnerOrigin: "synthetic", EditGeneration: 1}
	if _, _, e := s.dotOwnerAction(t.Context(), "alice", m.client.ID, request); e != errDotDisabled {
		t.Fatal("default must be off", e)
	}
	if _, e := s.db.Exec(`UPDATE config SET value='on' WHERE key='dot_enabled'`); e != nil {
		t.Fatal(e)
	}
	out, _, e := s.dotOwnerAction(t.Context(), "alice", m.client.ID, request)
	if e != nil || out.Binding.Status != "DISABLED" {
		t.Fatal(out, e)
	}
	bob := testModel(t, s, h, "bob")
	if _, _, e = s.dotOwnerAction(t.Context(), "bob", bob.client.ID, request); e == nil {
		t.Fatal("member became owner")
	}
	if _, _, e = s.dotOwnerAction(t.Context(), "alice", bob.client.ID, request); e == nil {
		t.Fatal("other session impersonation")
	}
}
func TestDotPrivateReviewExactCommitAndReceiptIdempotency(t *testing.T) {
	f := newDotFixture(t)
	op, confirm := f.draft(t)
	if dotAssistantCount(t, f.s) != 0 {
		t.Fatal("draft posted before confirmation")
	}
	status, e := f.s.dotGetOperation(t.Context(), f.identity, op.Ref)
	if e != nil || status.Status != "AWAITING_CONFIRMATION" {
		t.Fatal(status, e)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "Synthetic exact") {
		t.Fatal("MCP status leaked draft")
	}
	out, message, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, confirm)
	if e != nil || message == nil || out.Result.Status != "COMMITTED" || message.Sender != dotAssistantSender || message.Room != "dot-room" || message.ReplyTo != confirm.SourceID {
		t.Fatal(out, message, e)
	}
	if dotAssistantCount(t, f.s) != 1 {
		t.Fatal("no single committed message")
	}
	again, message, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, confirm)
	if e != nil || message != nil || again.Result.MessageID != out.Result.MessageID || dotAssistantCount(t, f.s) != 1 {
		t.Fatal("duplicate publication", again, message, e)
	}
	cancel := f.req("cancel")
	cancel.OperationRef = op.Ref
	after, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, cancel)
	if e != nil || after.Result.Status != "COMMITTED" {
		t.Fatal("false recall", after, e)
	}
}
func TestDotRejectConfirmationTamperingAndForeignPrivateReview(t *testing.T) {
	f := newDotFixture(t)
	op, confirm := f.draft(t)
	for _, mutate := range []func(*guiDotRequest){func(r *guiDotRequest) { r.BodySHA256 = dotHash("different") }, func(r *guiDotRequest) { r.AudienceSHA256 = dotHash("different") }, func(r *guiDotRequest) { r.SourceID++ }, func(r *guiDotRequest) { v := *r.ExpectedGeneration + 1; r.ExpectedGeneration = &v }, func(r *guiDotRequest) { r.ReviewToken = "forged" }, func(r *guiDotRequest) { r.Confirmed = false }, func(r *guiDotRequest) { r.Room = "lobby" }} {
		r := confirm
		mutate(&r)
		if _, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, r); e == nil {
			t.Fatal("tamper accepted", r)
		}
	}
	bob := testModel(t, f.s, f.h, "bob")
	r := f.req("review")
	r.OperationRef = op.Ref
	if _, _, e := f.s.dotOwnerAction(t.Context(), "bob", bob.client.ID, r); e == nil {
		t.Fatal("private draft leaked")
	}
	second := testModel(t, f.s, f.h, "alice")
	if _, _, e := f.s.dotOwnerAction(t.Context(), "alice", second.client.ID, confirm); e == nil {
		t.Fatal("review token crossed sessions")
	}
	if dotAssistantCount(t, f.s) != 0 {
		t.Fatal("tampering posted")
	}
}
func TestDotAudienceABAAndSourceEditRejectOldConfirmation(t *testing.T) {
	for _, change := range []string{"audience", "source", "epoch", "session", "oauth"} {
		t.Run(change, func(t *testing.T) {
			f := newDotFixture(t)
			_, confirm := f.draft(t)
			switch change {
			case "audience":
				if _, e := f.s.db.Exec(`INSERT INTO members VALUES('dot-room','bob','member',1);DELETE FROM members WHERE room='dot-room' AND user='bob'`); e != nil {
					t.Fatal(e)
				}
			case "source":
				if e := f.s.UpdateMessage("alice", confirm.SourceID, "@dot revised source", false); e != nil {
					t.Fatal(e)
				}
			case "epoch":
				if e := dotOne(f.s.db.Exec(`INSERT INTO credential_versions(user,epoch) VALUES('alice',1) ON CONFLICT(user) DO UPDATE SET epoch=epoch+1`)); e != nil {
					t.Fatal(e)
				}
			case "oauth":
				if e := dotOne(f.s.db.Exec(`UPDATE dot_oauth_links SET active=0 WHERE issuer=? AND subject=?`, f.identity.Issuer, f.identity.Subject)); e != nil {
					t.Fatal(e)
				}
			case "session":
				if _, e := f.s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, f.session); e != nil {
					t.Fatal(e)
				}
			}
			if _, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, confirm); e == nil {
				t.Fatal("stale confirmation accepted")
			}
			if dotAssistantCount(t, f.s) != 0 {
				t.Fatal("stale publication")
			}
			if change == "audience" || change == "oauth" {
				b, e := dotBindingQ(f.s.db, f.binding.Ref)
				if e != nil || b.Status != "PAUSED" {
					t.Fatal("ACL ABA did not pause", b, e)
				}
				if e = f.s.dotSubscribe(t.Context(), f.identity, dotSubscriptionInput{ID: "synthetic-sub", BindingRef: b.Ref, CallbackHash: dotHash("https://receiver.invalid/callback"), DeliveryHandle: "synthetic-vault-handle", Generation: b.Generation, Expires: time.Now().Unix() + 600}); e == nil {
					t.Fatal("refresh resumed")
				}
			}
		})
	}
}
func TestDotSourceIsolationNoLoopAndUnknownRecovery(t *testing.T) {
	f := newDotFixture(t)
	f.enable(t)
	for _, m := range []Message{{Room: "lobby", Sender: "alice", Kind: "chat", Body: "@dot wrong room"}, {Room: "dot-room", Sender: "bob", Kind: "chat", Body: "@dot not owner"}, {Room: "dot-room", Sender: "alice", Kind: "action", Body: "@dot system action"}} {
		// Bob gets explicit membership only before fixture enables? Public temporary
		// source fixture inserts avoid granting him permission to this private room.
		result, e := f.s.db.Exec(`INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(?,?,'',?,?,?)`, m.Room, m.Sender, m.Body, m.Kind, time.Now().Unix())
		if e != nil {
			t.Fatal(e)
		}
		id, _ := result.LastInsertId()
		ref, e := f.s.dotIngestSource(t.Context(), f.binding.Ref, id)
		if e != nil || ref != "" {
			t.Fatal("non-owner/cross-room/system source", ref, e)
		}
	}
	m, e := f.s.Send(Message{Room: "dot-room", Sender: "alice", Kind: "chat", Body: "@dot unknown delivery"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.dotIngestSource(t.Context(), f.binding.Ref, m.ID); e != nil {
		t.Fatal(e)
	}
	lease, e := f.s.dotClaimDelivery(t.Context(), f.binding.Ref)
	if e != nil || lease == nil {
		t.Fatal(lease, e)
	}
	if e = f.s.dotRecoverInterrupted(t.Context()); e != nil {
		t.Fatal(e)
	}
	if next, e := f.s.dotClaimDelivery(t.Context(), f.binding.Ref); e != nil || next != nil {
		t.Fatal("unknown blindly resent", next, e)
	}
	var status string
	if e = f.s.db.QueryRow(`SELECT status FROM dot_events WHERE ref=?`, lease.EventID).Scan(&status); e != nil || status != "UNKNOWN" {
		t.Fatal(status, e)
	}
}
func TestDotMandatoryWritesAndLaterMutationRollback(t *testing.T) {
	triggers := map[string]string{
		"ignore_message":    `CREATE TRIGGER synthetic_attack BEFORE INSERT ON messages WHEN new.kind='assistant' BEGIN SELECT RAISE(IGNORE);END;`,
		"ignore_receipt":    `CREATE TRIGGER synthetic_attack BEFORE UPDATE OF status ON dot_operations WHEN new.status='COMMITTED' BEGIN SELECT RAISE(IGNORE);END;`,
		"rewrite_body":      `CREATE TRIGGER synthetic_attack AFTER INSERT ON dot_message_origins BEGIN UPDATE messages SET body='tampered' WHERE id=new.message_id;END;`,
		"late_source":       `CREATE TRIGGER synthetic_attack AFTER INSERT ON audit WHEN new.action='dot.confirm' BEGIN UPDATE messages SET body='tampered source' WHERE id=(SELECT source_id FROM dot_operations WHERE ref=new.target);END;`,
		"late_acl":          `CREATE TRIGGER synthetic_attack AFTER INSERT ON audit WHEN new.action='dot.confirm' BEGIN INSERT INTO members VALUES('dot-room','bob','member',1);END;`,
		"late_oauth":        `CREATE TRIGGER synthetic_attack AFTER INSERT ON audit WHEN new.action='dot.confirm' BEGIN UPDATE dot_oauth_links SET active=0;END;`,
		"late_session":      `CREATE TRIGGER synthetic_attack AFTER INSERT ON audit WHEN new.action='dot.confirm' BEGIN UPDATE sessions SET revoked=1 WHERE user='alice';END;`,
		"duplicate_message": `CREATE TRIGGER synthetic_attack AFTER INSERT ON messages WHEN new.kind='assistant' BEGIN INSERT INTO messages(room,sender,recipient,body,kind,at) VALUES(new.room,new.sender,'',new.body,'assistant',new.at);END;`,
	}
	for name, sql := range triggers {
		t.Run(name, func(t *testing.T) {
			f := newDotFixture(t)
			op, confirm := f.draft(t)
			if _, e := f.s.db.Exec(sql); e != nil {
				t.Fatal(e)
			}
			if _, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, confirm); e == nil {
				t.Fatal("storage anomaly accepted")
			}
			if dotAssistantCount(t, f.s) != 0 {
				t.Fatal("partial publication persisted")
			}
			got, e := dotOperationQ(f.s.db, op.Ref)
			if e != nil || got.Status != "AWAITING_CONFIRMATION" {
				t.Fatal("receipt did not rollback", got, e)
			}
		})
	}
}
func TestDotIgnoredSendingIntentNeverReturnsDelivery(t *testing.T) {
	f := newDotFixture(t)
	f.enable(t)
	m, e := f.s.Send(Message{Room: "dot-room", Sender: "alice", Kind: "chat", Body: "@dot intent"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.dotIngestSource(t.Context(), f.binding.Ref, m.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.db.Exec(`CREATE TRIGGER synthetic_attack BEFORE UPDATE OF status ON dot_events WHEN new.status='SENDING' BEGIN SELECT RAISE(IGNORE);END;`); e != nil {
		t.Fatal(e)
	}
	if lease, e := f.s.dotClaimDelivery(t.Context(), f.binding.Ref); e == nil || lease != nil {
		t.Fatal("non-durable intent allowed send", lease, e)
	}
}
func TestDotWireRejectsMixedUnknownAndDuplicateFields(t *testing.T) {
	for _, raw := range []string{`{"operation":"review","operation":"confirm"}`, `{"operation":"review","target":"lobby"}`, `{"operation":"review","confirmed":"true"}`, `{"operation":"enable","expected_generation":true}`} {
		var r guiDotRequest
		if e := json.Unmarshal([]byte(raw), &r); e == nil {
			t.Fatal("invalid wire", raw)
		}
	}
	f := newDotFixture(t)
	r := f.req("list")
	request := guiRequest{Type: "dot", RequestID: "synthetic", Room: "dot-room", Dot: &r, Body: "must not cross into chat"}
	if _, e := guiDotRequestAction(t.Context(), f.s, f.h, f.u, guiScope{room: "dot-room"}, f.session, request); e == nil {
		t.Fatal("mixed sibling accepted")
	}
	request.Body = ""
	request.Room = "lobby"
	if _, e := guiDotRequestAction(t.Context(), f.s, f.h, f.u, guiScope{room: "lobby"}, f.session, request); e == nil {
		t.Fatal("navigated room accepted")
	}
}
func TestDotCancelBeforeCommitAndPauseInvalidates(t *testing.T) {
	f := newDotFixture(t)
	op, confirm := f.draft(t)
	cancel := f.req("cancel")
	cancel.OperationRef = op.Ref
	out, _, e := f.s.dotOwnerAction(t.Context(), "alice", f.session, cancel)
	if e != nil || out.Result.Status != "CANCELLED" {
		t.Fatal(out, e)
	}
	if _, _, e = f.s.dotOwnerAction(t.Context(), "alice", f.session, confirm); e == nil {
		t.Fatal("canceled operation published")
	}
	pause := f.req("pause")
	pause.BindingRef = f.binding.Ref
	pause.ExpectedGeneration = &f.binding.Generation
	out, _, e = f.s.dotOwnerAction(t.Context(), "alice", f.session, pause)
	if e != nil || out.Binding.Status != "PAUSED" {
		t.Fatal(out, e)
	}
	if dotAssistantCount(t, f.s) != 0 {
		t.Fatal("cancellation sent message")
	}
}

func TestDotPrivatePipeContractAndNoOwnerMethods(t *testing.T) {
	f := newDotFixture(t)
	if e := dotOne(f.s.db.Exec(`UPDATE config SET value='synthetic-installation' WHERE key='dot_installation_id'`)); e != nil {
		t.Fatal(e)
	}
	request := dotBridgeRequest{ID: "synthetic-pipe", Method: "profile", Identity: f.identity}
	raw, e := json.Marshal(request)
	if e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	if e = f.s.serveDotPrivatePipes(t.Context(), bytes.NewReader(append(raw, '\n')), &output); e != nil {
		t.Fatal(e)
	}
	var reply struct {
		ID     string            `json:"id"`
		Result map[string]string `json:"result"`
		Error  string            `json:"error"`
	}
	if e = json.Unmarshal(output.Bytes(), &reply); e != nil || reply.Error != "" || reply.Result["id"] != "synthetic-profile-alice" {
		t.Fatal(reply, e)
	}
	for _, method := range []string{"owner.confirm", "owner.enable", "send", "review"} {
		request.Method = method
		if _, e = f.s.dotBridgeDispatch(t.Context(), request); e == nil {
			t.Fatal("owner action exposed on adapter port", method)
		}
	}
	resolved, e := f.s.dotResolveIdentity(t.Context(), f.identity.Issuer, f.identity.Subject)
	if e != nil || resolved["tenant_id"] != "synthetic-installation" || resolved["auth_epoch"] != int64(0) {
		t.Fatal(resolved, e)
	}
}

func TestDotSourceMessageAndOutboxAreOneWriterTransaction(t *testing.T) {
	f := newDotFixture(t)
	f.enable(t)
	var before int
	if e := f.s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.db.Exec(`CREATE TRIGGER synthetic_ignore_event BEFORE INSERT ON dot_events BEGIN SELECT RAISE(IGNORE);END;`); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Send(Message{Room: "dot-room", Sender: "alice", Kind: "chat", Body: "@dot must durably enqueue"}); e == nil {
		t.Fatal("source committed without required outbox")
	}
	var after, events int
	if e := f.s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&after); e != nil {
		t.Fatal(e)
	}
	if e := f.s.db.QueryRow(`SELECT count(*) FROM dot_events`).Scan(&events); e != nil {
		t.Fatal(e)
	}
	if before != after || events != 0 {
		t.Fatal("source/outbox partial commit", before, after, events)
	}
	if _, e := f.s.db.Exec(`DROP TRIGGER synthetic_ignore_event`); e != nil {
		t.Fatal(e)
	}
	m, e := f.s.Send(Message{Room: "dot-room", Sender: "alice", Kind: "chat", Body: "@dot durably enqueued once"})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.db.QueryRow(`SELECT count(*) FROM dot_events WHERE source_id=? AND status='PENDING'`, m.ID).Scan(&events); e != nil || events != 1 {
		t.Fatal("source hook not durably attached", events, e)
	}
	if _, e = f.s.dotIngestSource(t.Context(), f.binding.Ref, m.ID); e != nil {
		t.Fatal(e)
	}
	if e = f.s.db.QueryRow(`SELECT count(*) FROM dot_events WHERE source_id=?`, m.ID).Scan(&events); e != nil || events != 1 {
		t.Fatal("duplicate source event", events, e)
	}
}
