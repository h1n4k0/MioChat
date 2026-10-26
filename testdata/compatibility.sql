CREATE TABLE users(name TEXT PRIMARY KEY,hash BLOB NOT NULL,salt BLOB NOT NULL,bio TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT '',theme TEXT NOT NULL DEFAULT 'aurora',role TEXT NOT NULL DEFAULT 'member',created INTEGER NOT NULL,banned INTEGER NOT NULL DEFAULT 0);
CREATE TABLE rooms(name TEXT PRIMARY KEY,topic TEXT NOT NULL,owner TEXT NOT NULL);
CREATE TABLE messages(id INTEGER PRIMARY KEY AUTOINCREMENT,room TEXT NOT NULL,sender TEXT NOT NULL,recipient TEXT NOT NULL DEFAULT '',body TEXT NOT NULL,kind TEXT NOT NULL,at INTEGER NOT NULL);
CREATE INDEX room_history ON messages(room,recipient,id);
CREATE INDEX dm_history ON messages(sender,recipient,id);
CREATE TABLE pins(room TEXT NOT NULL,message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,PRIMARY KEY(room,message_id));
INSERT INTO rooms VALUES('lobby','原有大厅',''),('dev','原有开发频道','');
