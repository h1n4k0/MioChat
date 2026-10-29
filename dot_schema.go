package main

// Add this additive step to the owner's next migration version, after the
// attachment migration if present. No credentials, host or external link created.
const schemaDotSQL = `
INSERT OR IGNORE INTO config(key,value) VALUES('dot_enabled','off'),('dot_installation_id','');
CREATE TABLE IF NOT EXISTS dot_room_epochs(room TEXT PRIMARY KEY,epoch INTEGER NOT NULL DEFAULT 1 CHECK(epoch>=1));
INSERT OR IGNORE INTO dot_room_epochs(room) SELECT name FROM rooms;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_insert AFTER INSERT ON rooms BEGIN
 INSERT INTO dot_room_epochs(room,epoch) VALUES(new.name,1) ON CONFLICT(room) DO UPDATE SET epoch=epoch+1;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_delete AFTER DELETE ON rooms BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.name;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_owner AFTER UPDATE OF owner ON rooms BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.name;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_meta_insert AFTER INSERT ON room_meta BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=new.name;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_meta_update AFTER UPDATE OF private,readonly,archived ON room_meta BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.name;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_meta_delete AFTER DELETE ON room_meta BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.name;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_member_insert AFTER INSERT ON members BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=new.room;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_member_update AFTER UPDATE ON members BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.room OR room=new.room;
END;
CREATE TRIGGER IF NOT EXISTS dot_room_epoch_member_delete AFTER DELETE ON members BEGIN
 UPDATE dot_room_epochs SET epoch=epoch+1 WHERE room=old.room;
END;
CREATE TABLE IF NOT EXISTS dot_bindings(
 ref TEXT PRIMARY KEY,owner_uid INTEGER NOT NULL REFERENCES user_meta(uid),owner_name TEXT NOT NULL,
 room TEXT NOT NULL,status TEXT NOT NULL,reason TEXT NOT NULL DEFAULT '',generation INTEGER NOT NULL,
 auth_epoch INTEGER NOT NULL,room_epoch INTEGER NOT NULL,watermark INTEGER NOT NULL,created INTEGER NOT NULL,
 UNIQUE(owner_uid,room));
CREATE TABLE IF NOT EXISTS dot_oauth_links(
 issuer TEXT NOT NULL,subject TEXT NOT NULL,owner_uid INTEGER NOT NULL REFERENCES user_meta(uid),
 auth_epoch INTEGER NOT NULL,profile_id TEXT NOT NULL UNIQUE,tokens_after INTEGER NOT NULL,active INTEGER NOT NULL,
 PRIMARY KEY(issuer,subject));
CREATE TABLE IF NOT EXISTS dot_subscriptions(
 id TEXT PRIMARY KEY,binding_ref TEXT NOT NULL UNIQUE REFERENCES dot_bindings(ref),generation INTEGER NOT NULL,
 callback_hash TEXT NOT NULL,delivery_handle TEXT NOT NULL,expires INTEGER NOT NULL,active INTEGER NOT NULL,
 issuer TEXT NOT NULL,subject TEXT NOT NULL,token_issued_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS dot_events(
 ref TEXT PRIMARY KEY,binding_ref TEXT NOT NULL REFERENCES dot_bindings(ref),generation INTEGER NOT NULL,
 source_id INTEGER NOT NULL REFERENCES messages(id),source_hash TEXT NOT NULL,created INTEGER NOT NULL,
 expires INTEGER NOT NULL,read_ref TEXT NOT NULL UNIQUE,reply_ref TEXT NOT NULL UNIQUE,status TEXT NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,lease_id TEXT NOT NULL DEFAULT '',
 UNIQUE(binding_ref,generation,source_id));
CREATE TABLE IF NOT EXISTS dot_operations(
 ref TEXT PRIMARY KEY,binding_ref TEXT NOT NULL REFERENCES dot_bindings(ref),generation INTEGER NOT NULL,
 event_ref TEXT NOT NULL UNIQUE REFERENCES dot_events(ref),idem TEXT NOT NULL,body TEXT NOT NULL,
 body_hash TEXT NOT NULL,source_id INTEGER NOT NULL,source_hash TEXT NOT NULL,room_epoch INTEGER NOT NULL,
 audience_hash TEXT NOT NULL,status TEXT NOT NULL,message_id INTEGER NOT NULL DEFAULT 0,
 created INTEGER NOT NULL,expires INTEGER NOT NULL,UNIQUE(binding_ref,idem));
CREATE TABLE IF NOT EXISTS dot_reviews(
 operation_ref TEXT NOT NULL REFERENCES dot_operations(ref),session_id TEXT NOT NULL REFERENCES sessions(id),
 token_hash TEXT NOT NULL,expires INTEGER NOT NULL,PRIMARY KEY(operation_ref,session_id));
CREATE TABLE IF NOT EXISTS dot_message_origins(
 message_id INTEGER PRIMARY KEY REFERENCES messages(id),event_ref TEXT NOT NULL UNIQUE REFERENCES dot_events(ref),
 operation_ref TEXT NOT NULL UNIQUE REFERENCES dot_operations(ref));
CREATE INDEX IF NOT EXISTS dot_events_pending ON dot_events(binding_ref,status,created);
CREATE INDEX IF NOT EXISTS dot_operations_pending ON dot_operations(binding_ref,status,created);
`
