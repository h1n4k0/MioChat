package main

// SQLite owns the bytes, offset, reservations and final message reference in
// the SAME transaction. There is deliberately no client-derived storage path,
// external blob rename, pending fsync, or dual file/database commit window.
const schemaAttachmentsSQL = `
CREATE TABLE IF NOT EXISTS attachment_writer_guard(id INTEGER PRIMARY KEY CHECK(id=1),value INTEGER NOT NULL);
INSERT OR IGNORE INTO attachment_writer_guard VALUES(1,0);
CREATE TABLE IF NOT EXISTS attachment_room_epochs(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL UNIQUE REFERENCES rooms(name) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS attachment_quota(owner_uid INTEGER NOT NULL,day TEXT NOT NULL,used INTEGER NOT NULL DEFAULT 0 CHECK(typeof(used)='integer' AND used>=0),reserved INTEGER NOT NULL DEFAULT 0 CHECK(typeof(reserved)='integer' AND reserved>=0),PRIMARY KEY(owner_uid,day),CHECK(used+reserved<=100000000));
CREATE TABLE IF NOT EXISTS attachment_uploads(
 id TEXT PRIMARY KEY,
 owner_uid INTEGER NOT NULL REFERENCES user_meta(uid) ON DELETE CASCADE,
 auth_epoch INTEGER NOT NULL,
 origin_session TEXT NOT NULL,
 intent_key TEXT NOT NULL,
 room TEXT NOT NULL,
 room_epoch INTEGER NOT NULL DEFAULT 0,
 recipient_uid INTEGER NOT NULL DEFAULT 0,
 filename TEXT NOT NULL,
 mime TEXT NOT NULL,
 size INTEGER NOT NULL CHECK(typeof(size)='integer' AND size BETWEEN 1 AND 20000000),
 sha256 TEXT NOT NULL,
 caption TEXT NOT NULL,
 day TEXT NOT NULL,
 expires_at INTEGER NOT NULL,
 accepted INTEGER NOT NULL DEFAULT 0 CHECK(typeof(accepted)='integer' AND accepted>=0 AND accepted<=size),
 state TEXT NOT NULL CHECK(state IN ('uploading','committed','cancelled','expired','rejected')),
 message_id INTEGER NOT NULL DEFAULT 0,
 width INTEGER NOT NULL DEFAULT 0,
 height INTEGER NOT NULL DEFAULT 0,
 frames INTEGER NOT NULL DEFAULT 0,
 UNIQUE(owner_uid,intent_key)
);
CREATE INDEX IF NOT EXISTS attachment_owner_pending ON attachment_uploads(owner_uid,state,expires_at);
CREATE INDEX IF NOT EXISTS attachment_expiry ON attachment_uploads(state,expires_at,id);
CREATE TABLE IF NOT EXISTS attachment_chunks(upload_id TEXT NOT NULL REFERENCES attachment_uploads(id) ON DELETE CASCADE,offset INTEGER NOT NULL CHECK(typeof(offset)='integer' AND offset>=0),data BLOB NOT NULL CHECK(typeof(data)='blob' AND length(data) BETWEEN 1 AND 24576),PRIMARY KEY(upload_id,offset));
CREATE TABLE IF NOT EXISTS message_attachments(message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,upload_id TEXT NOT NULL REFERENCES attachment_uploads(id) ON DELETE CASCADE,sender_uid INTEGER NOT NULL,recipient_uid INTEGER NOT NULL DEFAULT 0,room_epoch INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS message_attachments_blob ON message_attachments(upload_id,message_id);
CREATE TABLE IF NOT EXISTS attachment_reference_intents(owner_uid INTEGER NOT NULL REFERENCES user_meta(uid) ON DELETE CASCADE,intent_key TEXT NOT NULL,auth_epoch INTEGER NOT NULL,origin_session TEXT NOT NULL,source_message_id INTEGER NOT NULL,upload_id TEXT NOT NULL,size INTEGER NOT NULL CHECK(size BETWEEN 0 AND 20000000),room TEXT NOT NULL,room_epoch INTEGER NOT NULL,recipient_uid INTEGER NOT NULL,message_id INTEGER UNIQUE REFERENCES messages(id),created_at INTEGER NOT NULL,state TEXT NOT NULL DEFAULT 'committed' CHECK(state IN ('committed','rejected')),code TEXT NOT NULL DEFAULT '',PRIMARY KEY(owner_uid,intent_key),CHECK((state='committed' AND message_id IS NOT NULL AND size>0) OR (state='rejected' AND message_id IS NULL AND size=0)));
CREATE TRIGGER IF NOT EXISTS attachment_reference_intent_immutable BEFORE UPDATE ON attachment_reference_intents BEGIN SELECT RAISE(ABORT,'attachment reference intents are immutable'); END;
CREATE TRIGGER IF NOT EXISTS message_attachment_binding_immutable BEFORE UPDATE ON message_attachments BEGIN SELECT RAISE(ABORT,'message attachment binding is immutable'); END;
CREATE TRIGGER IF NOT EXISTS attachment_descriptor_immutable BEFORE UPDATE OF owner_uid,auth_epoch,origin_session,intent_key,room,room_epoch,recipient_uid,filename,mime,size,sha256,caption,day,expires_at ON attachment_uploads BEGIN SELECT RAISE(ABORT,'attachment descriptor is immutable'); END;
CREATE TRIGGER IF NOT EXISTS attachment_chunk_insert_bound BEFORE INSERT ON attachment_chunks WHEN NOT EXISTS(SELECT 1 FROM attachment_uploads u WHERE u.id=new.upload_id AND u.state='uploading' AND new.offset=u.accepted AND length(new.data)=min(24576,u.size-u.accepted)) BEGIN SELECT RAISE(ABORT,'attachment chunk is not the next bounded chunk'); END;
CREATE TRIGGER IF NOT EXISTS attachment_chunk_immutable BEFORE UPDATE ON attachment_chunks BEGIN SELECT RAISE(ABORT,'attachment chunks are immutable'); END;
CREATE TRIGGER IF NOT EXISTS attachment_chunk_live_no_delete BEFORE DELETE ON attachment_chunks WHEN EXISTS(SELECT 1 FROM attachment_uploads u WHERE u.id=old.upload_id AND u.state IN ('uploading','committed')) BEGIN SELECT RAISE(ABORT,'live attachment bytes cannot be removed'); END;
`
