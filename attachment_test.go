package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func attachmentFixture(t *testing.T) (*Store, User, string) {
	t.Helper()
	s := testStore(t)
	u := registerUser(t, s, "attachment-alice")
	id := "synthetic-attachment-session"
	if err := s.RecordSession(&Client{ID: id, Name: u.Name, AuthEpoch: u.AuthEpoch, IP: "192.0.2.40", Version: "attachment-synthetic"}); err != nil {
		t.Fatal(err)
	}
	return s, u, id
}

func attachmentReplaceUIDForTest(s *Store, u User) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		if err := changedOne(tx.Exec(`DELETE FROM user_meta WHERE uid=?`, u.UID)); err != nil {
			return err
		}
		return changedOne(tx.Exec(`INSERT INTO user_meta(name) VALUES(?)`, u.Name))
	})
}
func attachmentStart(t *testing.T, s *Store, u User, session, scope, key, name, mime string, data []byte) guiAttachmentReceipt {
	t.Helper()
	out, err := s.AttachmentForSession(t.Context(), u, session, scope, guiAttachmentRequest{Op: "intent", IntentKey: key, Filename: name, MIME: mime, Size: int64(len(data)), SHA256: attachmentDigest(data)})
	if err != nil || out.Status != "uploading" {
		t.Fatalf("start %#v %v", out, err)
	}
	return out
}
func attachmentSendBytes(t *testing.T, s *Store, u User, session, scope, id string, data []byte) guiAttachmentReceipt {
	t.Helper()
	var out guiAttachmentReceipt
	for offset := 0; offset < len(data); offset += attachmentChunkLimit {
		end := min(offset+attachmentChunkLimit, len(data))
		var err error
		out, err = s.AttachmentForSession(t.Context(), u, session, scope, guiAttachmentRequest{Op: "chunk", UploadID: id, Offset: int64(offset), DataB64: base64.StdEncoding.EncodeToString(data[offset:end])})
		if err != nil || out.AcceptedOffset != int64(end) {
			t.Fatalf("chunk %#v %v", out, err)
		}
	}
	return out
}
func attachmentFinish(t *testing.T, s *Store, u User, session, scope, id string) guiAttachmentReceipt {
	t.Helper()
	out, err := s.AttachmentForSession(t.Context(), u, session, scope, guiAttachmentRequest{Op: "commit", UploadID: id})
	if err != nil || out.Status != "committed" {
		t.Fatalf("commit %#v %v", out, err)
	}
	return out
}
func attachmentRequireCode(t *testing.T, err error, want string) {
	t.Helper()
	var typed attachmentError
	if !errors.As(err, &typed) || typed.code != want {
		t.Fatalf("want %s got %v", want, err)
	}
}

func TestAttachmentLifecycleChunksReplayLostACKFetchAndRecall(t *testing.T) {
	s, u, id := attachmentFixture(t)
	data := bytes.Repeat([]byte("synthetic-data\n"), 4000)
	start := attachmentStart(t, s, u, id, "lobby", "intent-first-001", "合成.txt", "text/plain", data)
	if start.Quota.Used != 0 || start.Quota.Reserved != int64(len(data)) {
		t.Fatal(start)
	}
	replay, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "intent", IntentKey: start.IntentKey, Filename: "合成.txt", MIME: "text/plain", Size: int64(len(data)), SHA256: attachmentDigest(data)})
	if err != nil || replay.UploadID != start.UploadID {
		t.Fatal(replay, err)
	}
	chunk := guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, DataB64: base64.StdEncoding.EncodeToString(data[:attachmentChunkLimit])}
	first, err := s.AttachmentForSession(t.Context(), u, id, "lobby", chunk)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.AttachmentForSession(t.Context(), u, id, "lobby", chunk)
	if err != nil || duplicate.Quota.Used != first.Quota.Used || duplicate.AcceptedOffset != first.AcceptedOffset {
		t.Fatal(duplicate, err)
	}
	chunk.DataB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), attachmentChunkLimit))
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", chunk)
	attachmentRequireCode(t, err, "chunk_conflict")
	last := attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, data)
	if last.Quota.Used != int64(len(data)) || last.Quota.Reserved != 0 {
		t.Fatal(last)
	}
	committed := attachmentFinish(t, s, u, id, "lobby", start.UploadID)
	for _, query := range []string{`UPDATE attachment_chunks SET data=x'00' WHERE upload_id=?`, `DELETE FROM attachment_chunks WHERE upload_id=?`, `UPDATE attachment_uploads SET size=size+1 WHERE id=?`} {
		if _, err := s.db.Exec(query, start.UploadID); err == nil {
			t.Fatal("immutable attachment storage changed", query)
		}
	}
	if !committed.NewCommit || committed.MessageID <= 0 || committed.Attachment.SHA256 != attachmentDigest(data) {
		t.Fatal(committed)
	}
	// Treat the successful ACK as lost: query original intent, never new intent.
	status, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", IntentKey: start.IntentKey})
	if err != nil || status.Status != "committed" || status.MessageID != committed.MessageID {
		t.Fatal(status, err)
	}
	dupe := attachmentFinish(t, s, u, id, "lobby", start.UploadID)
	if dupe.NewCommit || dupe.MessageID != committed.MessageID {
		t.Fatal("duplicate commit", dupe)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	late, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "cancel", UploadID: start.UploadID})
	if err != nil || late.Status != "committed" {
		t.Fatal("late cancel", late, err)
	}
	var downloaded []byte
	for offset := int64(0); ; {
		out, e := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "fetch", MessageID: committed.MessageID, Offset: offset})
		if e != nil {
			t.Fatal(e)
		}
		b, e := base64.StdEncoding.DecodeString(out.DataB64)
		if e != nil {
			t.Fatal(e)
		}
		downloaded = append(downloaded, b...)
		offset += int64(len(b))
		if !out.More {
			break
		}
	}
	if !bytes.Equal(downloaded, data) {
		t.Fatal("fetch changed bytes")
	}
	history, err := s.History("lobby", u.Name, "", 10)
	if err != nil || len(history) != 1 || history[0].Attachment == nil || history[0].Attachment.ID != start.UploadID {
		t.Fatal("metadata", history, err)
	}
	if _, err = s.db.Exec(`UPDATE message_meta SET recalled=1 WHERE id=?`, committed.MessageID); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "fetch", MessageID: committed.MessageID})
	attachmentRequireCode(t, err, "attachment_unavailable")
	history, err = s.History("lobby", u.Name, "", 10)
	if err != nil || history[0].Attachment != nil {
		t.Fatal("recalled metadata", history, err)
	}
}

func TestAttachmentCancelAcceptedChargeAndIntentConflicts(t *testing.T) {
	s, u, id := attachmentFixture(t)
	data := bytes.Repeat([]byte("a"), attachmentChunkLimit+11)
	start := attachmentStart(t, s, u, id, "lobby", "intent-cancel-001", "a.txt", "text/plain", data)
	_, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "intent", IntentKey: start.IntentKey, Filename: "b.txt", MIME: "text/plain", Size: int64(len(data)), SHA256: attachmentDigest(data)})
	attachmentRequireCode(t, err, "intent_conflict")
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, Offset: attachmentChunkLimit, DataB64: base64.StdEncoding.EncodeToString(data[attachmentChunkLimit:])})
	attachmentRequireCode(t, err, "offset_mismatch")
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, DataB64: "YQ=="})
	attachmentRequireCode(t, err, "chunk_invalid")
	attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, data[:attachmentChunkLimit])
	for range 2 {
		cancel, e := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "cancel", UploadID: start.UploadID})
		if e != nil || cancel.Status != "cancelled" || cancel.Quota.Used != attachmentChunkLimit || cancel.Quota.Reserved != 0 {
			t.Fatal(cancel, e)
		}
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM attachment_chunks WHERE upload_id=?`, start.UploadID).Scan(&n)
	if n != 0 {
		t.Fatal("partial bytes retained after cancel")
	}
}

func TestAttachmentExactDecimalLimitsAndConcurrentLastQuota(t *testing.T) {
	s, u, id := attachmentFixture(t)
	day, _ := attachmentDay(s.attachmentClock())
	request := guiAttachmentRequest{Op: "intent", IntentKey: "exact-item-0001", Filename: "x.txt", MIME: "text/plain", Size: attachmentItemLimit, SHA256: strings.Repeat("a", 64)}
	out, err := s.AttachmentForSession(t.Context(), u, id, "lobby", request)
	if err != nil || out.Size != 20000000 {
		t.Fatal(out, err)
	}
	request.IntentKey = "too-large-item-001"
	request.Size++
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", request)
	attachmentRequireCode(t, err, "intent_invalid")
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "cancel", UploadID: out.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE attachment_quota SET used=?,reserved=0 WHERE owner_uid=? AND day=?`, attachmentDailyLimit-30000, u.UID, day); err != nil {
		t.Fatal(err)
	}
	second := "synthetic-second-session"
	if err = s.RecordSession(&Client{ID: second, Name: u.Name, AuthEpoch: u.AuthEpoch}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, session := range []string{id, second} {
		wg.Add(1)
		go func(i int, session string) {
			defer wg.Done()
			_, err := s.AttachmentForSession(t.Context(), u, session, "lobby", guiAttachmentRequest{Op: "intent", IntentKey: fmt.Sprintf("race-intent-%08d", i), Filename: "r.txt", MIME: "text/plain", Size: 20000, SHA256: strings.Repeat("b", 64)})
			results <- err
		}(i, session)
	}
	wg.Wait()
	close(results)
	ok, bad := 0, 0
	for e := range results {
		if e == nil {
			ok++
		} else {
			attachmentRequireCode(t, e, "daily_quota")
			bad++
		}
	}
	if ok != 1 || bad != 1 {
		t.Fatal(ok, bad)
	}
	q, err := attachmentQuotaQ(s.db, u.UID, day)
	if err != nil || q.Used+q.Reserved != attachmentDailyLimit-10000 {
		t.Fatal(q, err)
	}
	request.IntentKey = "last-ten-thousand"
	request.Size = 10000
	out, err = s.AttachmentForSession(t.Context(), u, id, "lobby", request)
	if err != nil || out.Quota.Used+out.Quota.Reserved != 100000000 {
		t.Fatal(out, err)
	}
	request.IntentKey = "last-one-overflow"
	request.Size = 1
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", request)
	attachmentRequireCode(t, err, "daily_quota")
}

func TestAttachmentSessionEpochScopeACLAndPrivateFetch(t *testing.T) {
	s, u, id := attachmentFixture(t)
	bob := registerUser(t, s, "attachment-bob")
	eve := registerUser(t, s, "attachment-eve")
	for _, p := range []User{bob, eve} {
		if err := s.RecordSession(&Client{ID: p.Name + "-session", Name: p.Name, AuthEpoch: p.AuthEpoch}); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("private synthetic data")
	start := attachmentStart(t, s, u, id, "dm:"+bob.Name, "private-intent-01", "private.txt", "text/plain", data)
	_, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	attachmentRequireCode(t, err, "target_changed")
	foreign, err := s.AttachmentForSession(t.Context(), eve, eve.Name+"-session", "dm:"+u.Name, guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	if err != nil || foreign.Status != "unknown" || foreign.Attachment != nil {
		t.Fatal("upload leaked", foreign, err)
	}
	attachmentSendBytes(t, s, u, id, "dm:"+bob.Name, start.UploadID, data)
	committed := attachmentFinish(t, s, u, id, "dm:"+bob.Name, start.UploadID)
	got, err := s.AttachmentForSession(t.Context(), bob, bob.Name+"-session", "dm:"+u.Name, guiAttachmentRequest{Op: "fetch", MessageID: committed.MessageID})
	if err != nil || got.DataB64 != base64.StdEncoding.EncodeToString(data) {
		t.Fatal(got, err)
	}
	_, err = s.AttachmentForSession(t.Context(), eve, eve.Name+"-session", "dm:"+u.Name, guiAttachmentRequest{Op: "fetch", MessageID: committed.MessageID})
	if err == nil {
		t.Fatal("private fetch leaked")
	}
	start2 := attachmentStart(t, s, u, id, "lobby", "bound-session-01", "bound.txt", "text/plain", data)
	second := "another-valid-session"
	if err = s.RecordSession(&Client{ID: second, Name: u.Name, AuthEpoch: u.AuthEpoch}); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, second, "lobby", guiAttachmentRequest{Op: "chunk", UploadID: start2.UploadID, DataB64: base64.StdEncoding.EncodeToString(data)})
	attachmentRequireCode(t, err, "origin_session_changed")
	status, err := s.AttachmentForSession(t.Context(), u, second, "lobby", guiAttachmentRequest{Op: "status", UploadID: start2.UploadID})
	if err != nil || status.Status != "uploading" {
		t.Fatal(status, err)
	}
	if _, err = s.db.Exec(`UPDATE sessions SET revoked=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", UploadID: start2.UploadID})
	if !errors.Is(err, errCredentialsChanged) {
		t.Fatal(err)
	}
	if err = setCredentialEpochQ(s.db, u.Name, u.AuthEpoch+1); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, second, "lobby", guiAttachmentRequest{Op: "status", UploadID: start2.UploadID})
	if !errors.Is(err, errCredentialsChanged) {
		t.Fatal(err)
	}
}

func TestAttachmentRoomDeletionRecreationAndACLRevocation(t *testing.T) {
	s, u, id := attachmentFixture(t)
	owner := registerUser(t, s, "attachment-owner")
	if err := s.CreateRoom("attachment-private", owner.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room_meta SET private=1 WHERE name='attachment-private'; INSERT INTO members(room,user,role,joined) VALUES('attachment-private',?,'member',1)`, u.Name); err != nil {
		t.Fatal(err)
	}
	data := []byte("room synthetic")
	start := attachmentStart(t, s, u, id, "attachment-private", "private-room-001", "room.txt", "text/plain", data)
	if _, err := s.db.Exec(`DELETE FROM members WHERE room='attachment-private' AND user=?`, u.Name); err != nil {
		t.Fatal(err)
	}
	_, err := s.AttachmentForSession(t.Context(), u, id, "attachment-private", guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, DataB64: base64.StdEncoding.EncodeToString(data)})
	attachmentRequireCode(t, err, "target_denied")
	if _, err = s.db.Exec(`DELETE FROM members WHERE room='attachment-private';DELETE FROM room_meta WHERE name='attachment-private';DELETE FROM rooms WHERE name='attachment-private'`); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateRoom("attachment-private", u.Name); err != nil {
		t.Fatal(err)
	}
	attachmentStart(t, s, u, id, "attachment-private", "replacement-room-01", "new.txt", "text/plain", data)
	_, err = s.AttachmentForSession(t.Context(), u, id, "attachment-private", guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	attachmentRequireCode(t, err, "target_changed")
}

func TestAttachmentWriterQueueMidnightRejectsAndReleasesOnlyReserved(t *testing.T) {
	s, u, id := attachmentFixture(t)
	before := time.Date(2026, 10, 28, 23, 59, 59, 0, attachmentTimezone)
	var clock atomic.Int64
	clock.Store(before.Unix())
	s.attachmentNow = func() time.Time { return time.Unix(clock.Load(), 0) }
	data := bytes.Repeat([]byte("m"), attachmentChunkLimit+10)
	start := attachmentStart(t, s, u, id, "lobby", "midnight-intent-1", "midnight.txt", "text/plain", data)
	attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, data[:attachmentChunkLimit])
	if start.ExpiresAt != before.Add(time.Second).Unix() {
		t.Fatal("not capped at midnight", start)
	}
	lock, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	waitBefore := s.db.Stats().WaitCount
	done := make(chan guiAttachmentReceipt, 1)
	errs := make(chan error, 1)
	go func() {
		out, e := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, Offset: attachmentChunkLimit, DataB64: base64.StdEncoding.EncodeToString(data[attachmentChunkLimit:])})
		done <- out
		errs <- e
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == waitBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == waitBefore {
		lock.Rollback()
		t.Fatal("writer did not queue")
	}
	clock.Store(before.Add(time.Second).Unix())
	if err = lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	err = <-errs
	if err != nil || out.Status != "expired" || out.AcceptedOffset != attachmentChunkLimit || out.Quota.Used != attachmentChunkLimit || out.Quota.Reserved != 0 {
		t.Fatal(out, err)
	}
	limits, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "limits"})
	if err != nil || limits.Quota.Day != "2026-10-29" || limits.Quota.Used != 0 || limits.Quota.Reserved != 0 {
		t.Fatal(limits, err)
	}
}

func TestAttachmentFaultRollbackNoPhantomMessageAndRestart(t *testing.T) {
	s, u, id := attachmentFixture(t)
	data := bytes.Repeat([]byte("f"), attachmentChunkLimit+1)
	start := attachmentStart(t, s, u, id, "lobby", "fault-intent-001", "fault.txt", "text/plain", data)
	if _, err := s.db.Exec(`CREATE TRIGGER attachment_test_ignore BEFORE UPDATE ON attachment_quota BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatal(err)
	}
	_, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "chunk", UploadID: start.UploadID, DataB64: base64.StdEncoding.EncodeToString(data[:attachmentChunkLimit])})
	if err == nil {
		t.Fatal("ignored quota write succeeded")
	}
	if _, err = s.db.Exec(`DROP TRIGGER attachment_test_ignore`); err != nil {
		t.Fatal(err)
	}
	status, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	if err != nil || status.AcceptedOffset != 0 || status.Quota.Used != 0 || status.Quota.Reserved != int64(len(data)) {
		t.Fatal(status, err)
	}
	attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, data)
	if _, err = s.db.Exec(`CREATE TRIGGER attachment_test_ref_ignore BEFORE INSERT ON message_attachments BEGIN SELECT RAISE(IGNORE); END;`); err != nil {
		t.Fatal(err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "commit", UploadID: start.UploadID})
	if err == nil {
		t.Fatal("missing ref committed")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != 0 {
		t.Fatal("phantom message")
	}
	if _, err = s.db.Exec(`DROP TRIGGER attachment_test_ref_ignore`); err != nil {
		t.Fatal(err)
	}
	committed := attachmentFinish(t, s, u, id, "lobby", start.UploadID)
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	status, err = reopened.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", IntentKey: start.IntentKey})
	if err != nil || status.Status != "committed" || status.MessageID != committed.MessageID || status.Quota.Used != int64(len(data)) {
		t.Fatal(status, err)
	}
	got, err := reopened.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "fetch", MessageID: committed.MessageID})
	if err != nil || got.Attachment.SHA256 != attachmentDigest(data) {
		t.Fatal(got, err)
	}
}

func TestAttachmentContentAllowlistImageBudgetsAndInvalidCommit(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var p, j, g bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	pal := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White})
	if err := gif.EncodeAll(&g, &gif.GIF{Image: []*image.Paletted{pal, pal}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, mime string
		data       []byte
		frames     int
	}{{"a.png", "image/png", p.Bytes(), 1}, {"a.jpg", "image/jpeg", j.Bytes(), 1}, {"a.gif", "image/gif", g.Bytes(), 2}, {"a.txt", "text/plain", []byte("你好\nsynthetic"), 0}, {"a.md", "text/markdown", []byte("# synthetic"), 0}, {"a.csv", "text/csv", []byte("a,b\n1,2"), 0}, {"a.json", "application/json", []byte(`{"synthetic":true}`), 0}, {"a.pdf", "application/pdf", []byte("%PDF-1.7\nsynthetic opaque\n%%EOF\n"), 0}} {
		info, err := validateAttachmentContent(test.data, attachmentInfo{Filename: test.name, MIME: test.mime, Size: int64(len(test.data)), SHA256: attachmentDigest(test.data)})
		if err != nil || info.Frames != test.frames || info.MetadataPolicy != "preserve_original" {
			t.Fatal(test.name, info, err)
		}
	}
	for _, name := range []string{"../a.png", `C:\a.png`, "/a.png", "CON.png", "a.png\u202e", "a.png ", "a.svg", "a.exe", "a:png"} {
		if attachmentFilenameValid(name, "image/png") {
			t.Fatal("unsafe filename", name)
		}
	}
	badGIF := append([]byte(nil), g.Bytes()...)
	badGIF[6], badGIF[7], badGIF[8], badGIF[9] = 0xff, 0xff, 0xff, 0xff
	for _, data := range [][]byte{[]byte("not image"), p.Bytes()[:20], append(append([]byte(nil), p.Bytes()...), []byte("tail")...)} {
		if _, err := validateAttachmentContent(data, attachmentInfo{Filename: "bad.png", MIME: "image/png", Size: int64(len(data)), SHA256: attachmentDigest(data)}); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	if _, _, _, err := attachmentGIFBudget(badGIF); err == nil {
		t.Fatal("GIF dimensions bypass")
	}
	frames := make([]*image.Paletted, attachmentMaxFrames+1)
	delay := make([]int, len(frames))
	for i := range frames {
		frames[i] = pal
	}
	var many bytes.Buffer
	if err := gif.EncodeAll(&many, &gif.GIF{Image: frames, Delay: delay}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := attachmentGIFBudget(many.Bytes()); err == nil {
		t.Fatal("GIF frame budget bypass")
	}
	s, u, id := attachmentFixture(t)
	bad := []byte("not PNG bytes")
	start := attachmentStart(t, s, u, id, "lobby", "invalid-image-01", "bad.png", "image/png", bad)
	attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, bad)
	out, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "commit", UploadID: start.UploadID})
	if err != nil || out.Status != "rejected" || out.Code != "content_invalid" || out.Quota.Used != int64(len(bad)) || out.Quota.Reserved != 0 {
		t.Fatal(out, err)
	}
}

func TestAttachmentWireBudgetMixedRequestsAndUnknownLookup(t *testing.T) {
	s, u, id := attachmentFixture(t)
	request := guiRequest{Type: "attachment", RequestID: "synthetic-request", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "chunk", UploadID: strings.Repeat("a", 32), DataB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, attachmentChunkLimit))}}
	raw, err := json.Marshal(request)
	if err != nil || len(raw)+1 >= guiMaxFrameBytes {
		t.Fatal(len(raw), err)
	}
	request.Body = "unrelated chat"
	out := guiAttachmentOperation(t.Context(), s, u, guiScope{room: "lobby"}, id, request)
	if out.Attachment.Status != "error" || out.Attachment.Code != "request_invalid" {
		t.Fatal(out)
	}
	if out.Attachment.Op != "chunk" || out.Attachment.UploadID != request.Attachment.UploadID {
		t.Fatal("typed error lost operation correlation", out)
	}
	badJSON, e := json.Marshal(attachmentErrorReceipt(nil, "lobby", "request_invalid"))
	if e != nil || bytes.Contains(badJSON, []byte(`"op":""`)) {
		t.Fatal("empty enum breaks optional client decode", string(badJSON), e)
	}
	rate := attachmentErrorReceipt(request.Attachment, "lobby", "rate_limited")
	if rate.Op != "chunk" || rate.UploadID != request.Attachment.UploadID {
		t.Fatal(rate)
	}
	unknown, err := s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", IntentKey: "unknown-intent-001"})
	if err != nil || unknown.Status != "unknown" || unknown.Code != "not_found" || unknown.Attachment != nil {
		t.Fatal(unknown, err)
	}
	for _, bad := range []guiAttachmentRequest{{Op: "limits", DataB64: "unexpected"}, {Op: "cancel", UploadID: strings.Repeat("a", 32), Size: 1}, {Op: "status", UploadID: strings.Repeat("a", 32), IntentKey: "both-keys"}, {Op: "fetch", MessageID: 1, UploadID: strings.Repeat("a", 32)}} {
		if attachmentRequestValid(bad) {
			t.Fatal("mixed subfields", bad)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.AttachmentForSession(ctx, u, id, "lobby", guiAttachmentRequest{Op: "limits"}); err == nil {
		t.Fatal("cancelled context allowed")
	}
}

func TestAttachmentUIDMigrationAndSameNameReplacement(t *testing.T) {
	s, u, id := attachmentFixture(t)
	data := []byte("retired owner bytes")
	start := attachmentStart(t, s, u, id, "lobby", "retired-uid-0001", "old.txt", "text/plain", data)
	attachmentSendBytes(t, s, u, id, "lobby", start.UploadID, data)
	// Synthetic metadata replacement proves AUTOINCREMENT does not recycle a
	// retired UID. Production has no account-delete route added by this patch.
	if err := attachmentReplaceUIDForTest(s, u); err != nil {
		t.Fatal(err)
	}
	newUser, err := s.User(u.Name)
	if err != nil || newUser.UID <= u.UID {
		t.Fatal(newUser, err)
	}
	_, err = s.AttachmentForSession(t.Context(), u, id, "lobby", guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	if !errors.Is(err, errCredentialsChanged) {
		t.Fatal("old cached identity rebound", err)
	}
	status, err := s.AttachmentForSession(t.Context(), newUser, id, "lobby", guiAttachmentRequest{Op: "status", UploadID: start.UploadID})
	if err != nil || status.Status != "unknown" || status.Attachment != nil {
		t.Fatal(status, err)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
}

func TestAttachmentNativeWireNegotiationAndMessageReference(t *testing.T) {
	s := testStore(t)
	registerUser(t, s, "wire-attachment")
	var hello bytes.Buffer
	_ = runGUIProtocol(t.Context(), strings.NewReader(""), &hello, "192.0.2.42", "synthetic", func() {}, s, newHub())
	var first guiResponse
	if err := json.NewDecoder(&hello).Decode(&first); err != nil || !containsString(first.Capabilities, attachmentCapability) || !containsString(first.Capabilities, attachmentReferenceCapability) {
		t.Fatal(first, err)
	}
	f := newGUIWireFixture(t, s, newHub())
	f.send(guiRequest{Type: "attachment", RequestID: "before-auth", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "limits"}})
	f.read("error", "before-auth")
	f.send(guiRequest{Type: "login", RequestID: "auth-attachment", Username: "wire-attachment", Password: "test-password-123"})
	f.read("auth", "auth-attachment")
	f.read("state", "")
	data := bytes.Repeat([]byte("wire attachment synthetic\n"), 1200)
	f.send(guiRequest{Type: "attachment", RequestID: "wire-intent", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "intent", IntentKey: "wire-intent-0001", Filename: "wire.txt", MIME: "text/plain", Size: int64(len(data)), SHA256: attachmentDigest(data)}})
	intent := f.read("attachment", "wire-intent")
	if intent.Attachment == nil || intent.Attachment.Status != "uploading" {
		t.Fatal(intent)
	}
	for offset := 0; offset < len(data); offset += attachmentChunkLimit {
		requestID := fmt.Sprintf("wire-chunk-%d", offset)
		f.send(guiRequest{Type: "attachment", RequestID: requestID, Room: "lobby", Attachment: &guiAttachmentRequest{Op: "chunk", UploadID: intent.Attachment.UploadID, Offset: int64(offset), DataB64: base64.StdEncoding.EncodeToString(data[offset:min(offset+attachmentChunkLimit, len(data))])}})
		ack := f.read("attachment", requestID)
		if ack.Attachment == nil || ack.Attachment.AcceptedOffset != int64(min(offset+attachmentChunkLimit, len(data))) {
			t.Fatal(ack)
		}
	}
	f.send(guiRequest{Type: "attachment", RequestID: "wire-commit", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "commit", UploadID: intent.Attachment.UploadID}})
	commit := f.read("attachment", "wire-commit")
	if commit.Attachment == nil || commit.Attachment.Status != "committed" || commit.Message == nil || commit.Message.Attachment == nil || commit.Message.Attachment.ID != intent.Attachment.UploadID {
		t.Fatal(commit)
	}
	live := f.read("message", "")
	if live.Message == nil || live.Message.Attachment == nil || live.Message.ID != commit.Message.ID {
		t.Fatal(live)
	}
	f.send(guiRequest{Type: "attachment", RequestID: "wire-fetch", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "fetch", MessageID: commit.Message.ID}})
	fetch := f.read("attachment", "wire-fetch")
	if fetch.Attachment == nil || fetch.Attachment.Status != "fetched" || fetch.Attachment.Offset != 0 || !fetch.Attachment.More {
		t.Fatal(fetch)
	}
	encoded, err := json.Marshal(fetch)
	if err != nil || len(encoded)+1 >= guiMaxFrameBytes {
		t.Fatal(len(encoded), err)
	}
	f.send(guiRequest{Type: "attachment", RequestID: "wire-reference", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "reference", IntentKey: "wire-reference-001", MessageID: commit.Message.ID}})
	ref := f.read("attachment", "wire-reference")
	if ref.Attachment == nil || ref.Attachment.Status != "committed" || ref.Attachment.SourceMessageID != commit.Message.ID || ref.Attachment.AcceptedOffset != 0 || ref.Message == nil || ref.Message.Kind != "attachment" || ref.Message.Attachment == nil || ref.Message.Attachment.ID != commit.Message.Attachment.ID {
		t.Fatal(ref)
	}
	f.read("message", "")
	f.send(guiRequest{Type: "attachment", RequestID: "wire-reference-status", Room: "lobby", Attachment: &guiAttachmentRequest{Op: "status", IntentKey: "wire-reference-001"}})
	refStatus := f.read("attachment", "wire-reference-status")
	if refStatus.Attachment == nil || refStatus.Attachment.SourceMessageID != commit.Message.ID || refStatus.Attachment.MessageID != ref.Attachment.MessageID {
		t.Fatal(refStatus)
	}
}

func TestAttachmentRetiredTargetsDoNotLeakBodyOrQuotedMetadata(t *testing.T) {
	s, u, id := attachmentFixture(t)
	bob := registerUser(t, s, "retired-bob")
	data := []byte("retired synthetic file")
	start := attachmentStart(t, s, u, id, "dm:"+bob.Name, "retired-private-001", "private-secret-name.txt", "text/plain", data)
	attachmentSendBytes(t, s, u, id, "dm:"+bob.Name, start.UploadID, data)
	committed := attachmentFinish(t, s, u, id, "dm:"+bob.Name, start.UploadID)
	if _, err := s.Send(Message{Sender: u.Name, Recipient: bob.Name, Kind: "chat", Body: "reply without private metadata", ReplyTo: committed.MessageID}); err != nil {
		t.Fatal(err)
	}
	if err := attachmentReplaceUIDForTest(s, bob); err != nil {
		t.Fatal(err)
	}
	newBob, err := s.User(bob.Name)
	if err != nil || newBob.UID == bob.UID {
		t.Fatal(newBob, err)
	}
	if _, err = s.Message(newBob.Name, committed.MessageID); err == nil {
		t.Fatal("same-name account inherited private message")
	}
	history, err := s.History("", newBob.Name, u.Name, 10)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	for _, m := range history {
		if m.Attachment != nil || strings.Contains(m.Body, "private-secret-name") || strings.Contains(m.ReplyBody, "private-secret-name") {
			t.Fatal("retired recipient leaked metadata", m)
		}
	}
	if err = s.CreateRoom("retired-room", u.Name); err != nil {
		t.Fatal(err)
	}
	start = attachmentStart(t, s, u, id, "retired-room", "retired-room-0001", "old-room-secret.txt", "text/plain", data)
	attachmentSendBytes(t, s, u, id, "retired-room", start.UploadID, data)
	committed = attachmentFinish(t, s, u, id, "retired-room", start.UploadID)
	if _, err = s.db.Exec(`DELETE FROM members WHERE room='retired-room';DELETE FROM room_meta WHERE name='retired-room';DELETE FROM rooms WHERE name='retired-room'`); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateRoom("retired-room", u.Name); err != nil {
		t.Fatal(err)
	}
	attachmentStart(t, s, u, id, "retired-room", "new-room-00000001", "new-room.txt", "text/plain", data)
	if _, err = s.Message(u.Name, committed.MessageID); err == nil {
		t.Fatal("same-name room inherited attachment")
	}
	history, err = s.History("retired-room", u.Name, "", 10)
	if err != nil || len(history) != 1 || history[0].Attachment != nil || history[0].Body != "[附件不可用]" {
		t.Fatal(history, err)
	}
}

// Plain text must not spend the shared page deadline on an empty attachment
// query, and no preexisting attachment descriptor may survive this fast path.
func TestAttachmentMetadataSkipsPlainPagesAndClearsEveryDescriptor(t *testing.T) {
	s := testStore(t)
	first, err := s.readDB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.readDB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	stale := &attachmentInfo{ID: "stale-descriptor", Filename: "stale.txt"}
	messages := []Message{{ID: 1, Kind: "chat", Body: "plain", ReplyBody: "visible reply", Attachment: stale}, {ID: 2, Kind: "system", Body: "notice", Attachment: stale}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.decorateAttachmentsContext(ctx, messages, "alice"); err != nil {
		t.Fatal("plain page performed an unnecessary read", err)
	}
	if messages[0].Attachment != nil || messages[1].Attachment != nil || messages[0].Body != "plain" || messages[0].ReplyBody != "visible reply" || messages[1].Body != "notice" {
		t.Fatal("plain projection changed or retained stale metadata", messages)
	}
	// A real attachment cannot use that fast path or its stale cached descriptor.
	messages = append(messages, Message{ID: 3, Kind: "attachment", Body: "private filename", Attachment: stale})
	if err = s.decorateAttachmentsContext(ctx, messages, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatal("mixed page bypassed its current read", err)
	}
	for _, m := range messages {
		if m.Attachment != nil {
			t.Fatal("read failure retained descriptor", m.ID)
		}
	}
}

func TestAttachmentMixedPageRefreshesACLAndQuotedSource(t *testing.T) {
	s, alice, session := attachmentFixture(t)
	bob := registerUser(t, s, "mixed-page-bob")
	if err := s.CreateRoomAccess("mixed-page-private", alice.Name, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RoomMember(alice.Name, "mixed-page-private", bob.Name, "invite"); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic mixed-page attachment")
	start := attachmentStart(t, s, alice, session, "mixed-page-private", "mixed-page-source", "private-name.txt", "text/plain", data)
	attachmentSendBytes(t, s, alice, session, "mixed-page-private", start.UploadID, data)
	committed := attachmentFinish(t, s, alice, session, "mixed-page-private", start.UploadID)
	reply, err := s.Send(Message{Sender: alice.Name, Room: "mixed-page-private", Kind: "chat", Body: "ordinary reply", ReplyTo: committed.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.History("mixed-page-private", bob.Name, "", 10)
	if err != nil || len(page) != 2 || page[0].ID != committed.MessageID || page[0].Kind != "attachment" || page[0].Attachment == nil || page[0].Attachment.SHA256 != attachmentDigest(data) || page[1].ID != reply.ID || page[1].Attachment != nil || page[1].ReplyBody == "" {
		t.Fatal("mixed page lost allowed metadata/reply", page, err)
	}
	// Refresh previously visible values after actual ACL revocation. Ordinary
	// reply text remains text; its quoted attachment and the attachment itself
	// must not retain the cached private descriptor or filename.
	page[1].Attachment = page[0].Attachment
	if err = s.RoomMember(alice.Name, "mixed-page-private", bob.Name, "remove"); err != nil {
		t.Fatal(err)
	}
	if err = s.decorateContext(t.Context(), page, bob.Name); err != nil {
		t.Fatal(err)
	}
	if page[0].Attachment != nil || page[0].Body != "[附件不可用]" || page[0].ReplyBody != "" || page[0].ReplyName != "" || page[1].Attachment != nil || page[1].Body != "ordinary reply" || page[1].ReplyBody != "" || page[1].ReplyName != "" {
		t.Fatal("mixed page retained revoked attachment/quote", page)
	}
	if _, err = s.History("mixed-page-private", bob.Name, "", 10); err == nil {
		t.Fatal("history bypassed revoked room ACL")
	}
	if err = s.RoomMember(alice.Name, "mixed-page-private", bob.Name, "invite"); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateMessage(alice.Name, committed.MessageID, "", true); err != nil {
		t.Fatal(err)
	}
	page, err = s.History("mixed-page-private", bob.Name, "", 10)
	if err != nil || len(page) != 2 || page[0].Recalled == 0 || page[0].Body != "[消息已撤回]" || page[0].Attachment != nil || page[1].Attachment != nil || page[1].ReplyBody != "[消息已撤回]" {
		t.Fatal("recall lost in mixed page", page, err)
	}
}
