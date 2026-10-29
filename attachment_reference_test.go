package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func attachmentSessionForTest(t *testing.T, s *Store, u User, id string) string {
	t.Helper()
	if err := s.RecordSession(&Client{ID: id, Name: u.Name, AuthEpoch: u.AuthEpoch}); err != nil {
		t.Fatal(err)
	}
	return id
}
func attachmentReferenceForTest(t *testing.T, s *Store, u User, session, scope, key string, source int64) guiAttachmentReceipt {
	t.Helper()
	out, err := s.AttachmentForSession(t.Context(), u, session, scope, guiAttachmentRequest{Op: "reference", IntentKey: key, MessageID: source})
	if err != nil || out.Status != "committed" || out.Attachment == nil {
		t.Fatal(out, err)
	}
	return out
}

func TestAttachmentReferenceZeroChargeIdempotentConcurrentAndCrossTarget(t *testing.T) {
	s, alice, session := attachmentFixture(t)
	data := []byte("synthetic reusable bytes")
	start := attachmentStart(t, s, alice, session, "lobby", "reference-source-001", "reuse.txt", "text/plain", data)
	attachmentSendBytes(t, s, alice, session, "lobby", start.UploadID, data)
	original := attachmentFinish(t, s, alice, session, "lobby", start.UploadID)
	bob := registerUser(t, s, "reference-bob")
	b1 := attachmentSessionForTest(t, s, bob, "ref-bob-one")
	b2 := attachmentSessionForTest(t, s, bob, "ref-bob-two")
	if err := s.CreateRoom("ref-target", bob.Name); err != nil {
		t.Fatal(err)
	}
	day, _ := attachmentDay(s.attachmentClock())
	if _, err := s.db.Exec(`INSERT INTO attachment_quota(owner_uid,day,used,reserved) VALUES(?,?,?,0)`, bob.UID, day, attachmentDailyLimit); err != nil {
		t.Fatal(err)
	}
	var beforeChunks int
	if err := s.db.QueryRow(`SELECT count(*) FROM attachment_chunks`).Scan(&beforeChunks); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan guiAttachmentReceipt, 2)
	errs := make(chan error, 2)
	for _, id := range []string{b1, b2} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			out, e := s.AttachmentForSession(t.Context(), bob, id, "ref-target", guiAttachmentRequest{Op: "reference", IntentKey: "same-reference-001", MessageID: original.MessageID})
			results <- out
			errs <- e
		}(id)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var ref guiAttachmentReceipt
	created := 0
	for out := range results {
		if out.Status != "committed" || out.SourceMessageID != original.MessageID || out.OwnerUID != bob.UID || out.UploadID != start.UploadID || out.AcceptedOffset != 0 || out.Size != int64(len(data)) || out.MessageID == original.MessageID || out.Quota.Used != attachmentDailyLimit || out.Quota.Reserved != 0 {
			t.Fatal(out)
		}
		if ref.MessageID != 0 && ref.MessageID != out.MessageID {
			t.Fatal("duplicate reference messages")
		}
		ref = out
		if out.NewCommit {
			created++
		}
	}
	if created != 1 {
		t.Fatal("reference broadcast count", created)
	}
	status, err := s.AttachmentForSession(t.Context(), bob, b2, "ref-target", guiAttachmentRequest{Op: "status", IntentKey: "same-reference-001"})
	if err != nil || status.SourceMessageID != original.MessageID || status.MessageID != ref.MessageID || status.AcceptedOffset != 0 || status.NewCommit {
		t.Fatal(status, err)
	}
	fetched, err := s.AttachmentForSession(t.Context(), alice, session, "ref-target", guiAttachmentRequest{Op: "fetch", MessageID: ref.MessageID})
	if err != nil || fetched.DataB64 != base64.StdEncoding.EncodeToString(data) {
		t.Fatal(fetched, err)
	}
	var uploads, refs, chunks int
	s.db.QueryRow(`SELECT count(*) FROM attachment_uploads`).Scan(&uploads)
	s.db.QueryRow(`SELECT count(*) FROM message_attachments`).Scan(&refs)
	s.db.QueryRow(`SELECT count(*) FROM attachment_chunks`).Scan(&chunks)
	if uploads != 1 || refs != 2 || chunks != beforeChunks {
		t.Fatal(uploads, refs, chunks, beforeChunks)
	}
	_, err = s.AttachmentForSession(t.Context(), bob, b1, "lobby", guiAttachmentRequest{Op: "reference", IntentKey: "same-reference-001", MessageID: original.MessageID})
	attachmentRequireCode(t, err, "intent_conflict")
	_, err = s.AttachmentForSession(t.Context(), bob, b1, "ref-target", guiAttachmentRequest{Op: "intent", IntentKey: "same-reference-001", Filename: "x.txt", MIME: "text/plain", Size: 1, SHA256: attachmentDigest([]byte("x"))})
	attachmentRequireCode(t, err, "intent_conflict")
	_, err = s.AttachmentForSession(t.Context(), alice, session, "ref-target", guiAttachmentRequest{Op: "reference", IntentKey: start.IntentKey, MessageID: original.MessageID})
	attachmentRequireCode(t, err, "intent_conflict")
	if _, err = s.db.Exec(`DELETE FROM members WHERE room='ref-target';DELETE FROM room_meta WHERE name='ref-target';DELETE FROM rooms WHERE name='ref-target'`); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateRoom("ref-target", bob.Name); err != nil {
		t.Fatal(err)
	}
	fresh := attachmentReferenceForTest(t, s, bob, b1, "ref-target", "new-target-reference", original.MessageID)
	if fresh.MessageID == ref.MessageID {
		t.Fatal("recreated target adopted old reference")
	}
	_, err = s.AttachmentForSession(t.Context(), bob, b1, "ref-target", guiAttachmentRequest{Op: "status", IntentKey: "same-reference-001"})
	attachmentRequireCode(t, err, "target_changed")
	if _, err = s.Message(bob.Name, ref.MessageID); err == nil {
		t.Fatal("retired reference room inherited body")
	}

}

func TestAttachmentReferenceDurableRejectionAndOriginalRecallRevokesCopies(t *testing.T) {
	s, alice, session := attachmentFixture(t)
	bob := registerUser(t, s, "copy-reader")
	eve := registerUser(t, s, "copy-outsider")
	bs := attachmentSessionForTest(t, s, bob, "copy-reader-session")
	es := attachmentSessionForTest(t, s, eve, "copy-outsider-session")
	if err := s.CreateRoomAccess("copy-private", alice.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO members(room,user,role,joined) VALUES('copy-private',?,'member',1)`, bob.Name); err != nil {
		t.Fatal(err)
	}
	data := []byte("private original fixture")
	start := attachmentStart(t, s, alice, session, "copy-private", "copy-private-src", "hidden-name.txt", "text/plain", data)
	attachmentSendBytes(t, s, alice, session, "copy-private", start.UploadID, data)
	original := attachmentFinish(t, s, alice, session, "copy-private", start.UploadID)
	rejected, err := s.AttachmentForSession(t.Context(), eve, es, "dev", guiAttachmentRequest{Op: "reference", IntentKey: "denied-source-ref", MessageID: original.MessageID})
	if err != nil || rejected.Status != "rejected" || rejected.Code != "source_unavailable" || rejected.Attachment != nil || rejected.MessageID != 0 || rejected.SourceMessageID != original.MessageID || rejected.OwnerUID != eve.UID {
		t.Fatal(rejected, err)
	}
	rejectedStatus, err := s.AttachmentForSession(t.Context(), eve, es, "dev", guiAttachmentRequest{Op: "status", IntentKey: "denied-source-ref"})
	if err != nil || rejectedStatus.Status != "rejected" || rejectedStatus.Code != "source_unavailable" || rejectedStatus.Attachment != nil {
		t.Fatal(rejectedStatus, err)
	}
	ref := attachmentReferenceForTest(t, s, bob, bs, "dev", "allowed-copy-ref", original.MessageID)
	if _, err = s.db.Exec(`DELETE FROM members WHERE room='copy-private' AND user=?`, bob.Name); err != nil {
		t.Fatal(err)
	}
	denied, err := s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "reference", IntentKey: "revoked-source-ref", MessageID: original.MessageID})
	if err != nil || denied.Status != "rejected" {
		t.Fatal(denied, err)
	}
	// This explicit copy has its own destination ACL; leaving the original room
	// does not retroactively authorize other copies, or remove this published one.
	if _, err = s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "fetch", MessageID: ref.MessageID}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE message_meta SET recalled=1 WHERE id=?`, original.MessageID); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "fetch", MessageID: ref.MessageID})
	attachmentRequireCode(t, err, "attachment_unavailable")
	status, err := s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "status", IntentKey: "allowed-copy-ref"})
	if err != nil || status.Status != "committed" || status.Code != "reference_unavailable" || status.MessageID != ref.MessageID || status.Attachment != nil {
		t.Fatal("committed copy must not become rollback", status, err)
	}
	history, err := s.History("dev", bob.Name, "", 10)
	if err != nil || len(history) != 1 || history[0].Body != "[附件不可用]" || history[0].Attachment != nil {
		t.Fatal(history, err)
	}
	denied, err = s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "reference", IntentKey: "copy-withdrawn-ref", MessageID: ref.MessageID})
	if err != nil || denied.Status != "rejected" {
		t.Fatal(denied, err)
	}
}

func TestAttachmentReferencePrivateDestinationAndMandatoryIntentRollback(t *testing.T) {
	s, alice, session := attachmentFixture(t)
	bob := registerUser(t, s, "private-copy-bob")
	eve := registerUser(t, s, "private-copy-eve")
	bs := attachmentSessionForTest(t, s, bob, "private-copy-bob-s")
	es := attachmentSessionForTest(t, s, eve, "private-copy-eve-s")
	data := []byte("private destination fixture")
	start := attachmentStart(t, s, alice, session, "lobby", "private-copy-src", "copy.txt", "text/plain", data)
	attachmentSendBytes(t, s, alice, session, "lobby", start.UploadID, data)
	original := attachmentFinish(t, s, alice, session, "lobby", start.UploadID)
	if _, err := s.db.Exec(`CREATE TRIGGER reference_test_ignore BEFORE INSERT ON attachment_reference_intents BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatal(err)
	}
	_, err := s.AttachmentForSession(t.Context(), bob, bs, "dm:"+eve.Name, guiAttachmentRequest{Op: "reference", IntentKey: "private-copy-key", MessageID: original.MessageID})
	if err == nil {
		t.Fatal("missing reference intent reported success")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != 1 {
		t.Fatal("phantom copy", n)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reference_test_ignore`); err != nil {
		t.Fatal(err)
	}
	unknown, err := s.AttachmentForSession(t.Context(), bob, bs, "dm:"+eve.Name, guiAttachmentRequest{Op: "status", IntentKey: "private-copy-key"})
	if err != nil || unknown.Status != "unknown" || unknown.Code != "not_found" {
		t.Fatal(unknown, err)
	}
	ref := attachmentReferenceForTest(t, s, bob, bs, "dm:"+eve.Name, "private-copy-key", original.MessageID)
	fetched, err := s.AttachmentForSession(t.Context(), eve, es, "dm:"+bob.Name, guiAttachmentRequest{Op: "fetch", MessageID: ref.MessageID})
	if err != nil || fetched.DataB64 != base64.StdEncoding.EncodeToString(data) {
		t.Fatal(fetched, err)
	}
	if _, err = s.AttachmentForSession(t.Context(), alice, session, "dm:"+eve.Name, guiAttachmentRequest{Op: "fetch", MessageID: ref.MessageID}); err == nil {
		t.Fatal("blob owner gained other people's private-copy conversation")
	}
	// Recalling one copied message never changes the original byte storage/ref.
	if _, err = s.db.Exec(`UPDATE message_meta SET recalled=1 WHERE id=?`, ref.MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AttachmentForSession(t.Context(), alice, session, "lobby", guiAttachmentRequest{Op: "fetch", MessageID: original.MessageID}); err != nil {
		t.Fatal(err)
	}
	if err = attachmentReplaceUIDForTest(s, eve); err != nil {
		t.Fatal(err)
	}
	newEve, err := s.User(eve.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Message(newEve.Name, ref.MessageID); err == nil {
		t.Fatal("same-name endpoint inherited private reference")
	}
	surviving := attachmentReferenceForTest(t, s, bob, bs, "dev", "owner-delete-ref", original.MessageID)
	if err = attachmentReplaceUIDForTest(s, alice); err != nil {
		t.Fatal(err)
	}
	gone, err := s.AttachmentForSession(t.Context(), bob, bs, "dev", guiAttachmentRequest{Op: "status", IntentKey: "owner-delete-ref"})
	if err != nil || gone.Status != "committed" || gone.Code != "reference_unavailable" || gone.Attachment != nil || gone.MessageID != surviving.MessageID {
		t.Fatal("retired blob owner outcome", gone, err)
	}
	if _, err = s.Message(bob.Name, surviving.MessageID); err == nil {
		t.Fatal("retired blob owner left visible reference")
	}

}

func TestAttachmentGlobalExpiryBoundedAcrossOwnersRollbackAndStartup(t *testing.T) {
	s, alice, session := attachmentFixture(t)
	bob := registerUser(t, s, "orphan-bob")
	bs := attachmentSessionForTest(t, s, bob, "orphan-bob-session")
	base := time.Now().Add(-time.Hour)
	var clock atomic.Int64
	clock.Store(base.Unix())
	s.attachmentNow = func() time.Time { return time.Unix(clock.Load(), 0) }
	data := bytes.Repeat([]byte("q"), attachmentChunkLimit+10)
	for i, actor := range []User{alice, alice, bob} {
		sid := session
		if actor.UID == bob.UID {
			sid = bs
		}
		st := attachmentStart(t, s, actor, sid, "lobby", fmt.Sprintf("global-orphan-%02d", i), "orphan.txt", "text/plain", data)
		attachmentSendBytes(t, s, actor, sid, "lobby", st.UploadID, data[:attachmentChunkLimit])
	}
	clock.Store(base.Add(10 * time.Minute).Unix())
	fresh := attachmentStart(t, s, alice, session, "lobby", "not-expired-yet", "fresh.txt", "text/plain", []byte("x"))
	clock.Store(base.Add(31 * time.Minute).Unix())
	if _, err := s.db.Exec(`CREATE TRIGGER expiry_test_ignore BEFORE UPDATE ON attachment_quota BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireAttachmentUploads(t.Context(), 1); err == nil || n != 0 {
		t.Fatal("failed cleanup reported reclaimed", n, err)
	}
	var pending int
	s.db.QueryRow(`SELECT count(*) FROM attachment_uploads WHERE state='uploading'`).Scan(&pending)
	if pending != 4 {
		t.Fatal("cleanup failure changed state", pending)
	}
	if _, err := s.db.Exec(`DROP TRIGGER expiry_test_ignore`); err != nil {
		t.Fatal(err)
	}
	n, err := s.ExpireAttachmentUploads(t.Context(), 1)
	if err != nil || n != 1 {
		t.Fatal("bounded first batch", n, err)
	}
	n, err = s.ExpireAttachmentUploads(t.Context(), 64)
	if err != nil || n != 2 {
		t.Fatal("cross-owner batch", n, err)
	}
	n, err = s.ExpireAttachmentUploads(t.Context(), 64)
	if err != nil || n != 0 {
		t.Fatal("idempotent sweep", n, err)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM attachment_uploads WHERE id=?`, fresh.UploadID).Scan(&state); err != nil || state != "uploading" {
		t.Fatal(state, err)
	}
	day, _ := attachmentDay(base)
	aq, err := attachmentQuotaQ(s.db, alice.UID, day)
	if err != nil || aq.Used != 2*attachmentChunkLimit || aq.Reserved != 1 {
		t.Fatal(aq, err)
	}
	bq, err := attachmentQuotaQ(s.db, bob.UID, day)
	if err != nil || bq.Used != attachmentChunkLimit || bq.Reserved != 0 {
		t.Fatal(bq, err)
	}
	var chunks int
	s.db.QueryRow(`SELECT count(*) FROM attachment_chunks`).Scan(&chunks)
	if chunks != 0 {
		t.Fatal("expired partial bytes retained", chunks)
	}
	// The remaining fixture is already expired under real startup time, although
	// it was intentionally not expired under the earlier injected sweep clock.
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	if err = reopened.db.QueryRow(`SELECT state FROM attachment_uploads WHERE id=?`, fresh.UploadID).Scan(&state); err != nil || state != "expired" {
		t.Fatal("startup orphan sweep", state, err)
	}
	aq, err = attachmentQuotaQ(reopened.db, alice.UID, day)
	if err != nil || aq.Used != 2*attachmentChunkLimit || aq.Reserved != 0 {
		t.Fatal(aq, err)
	}
}
