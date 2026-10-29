package main

import (
	"database/sql"
	"errors"
)

type guiRoomPreferences struct {
	Pinned     bool `json:"pinned"`
	Subscribed bool `json:"subscribed"`
	Muted      bool `json:"muted"`
	Unread     int  `json:"unread"`
}

func guiRoomPreference(s *Store, actor, sessionID, room, key string, on bool) error {
	if key != "pinned" && key != "subscribed" && key != "muted" {
		return errors.New("invalid room preference")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, err := mutationActorQ(tx, actor)
		if err != nil {
			return err
		}
		if sessionID != "" {
			if valid, e := sessionCurrentQ(tx, sessionID, u.Name); e != nil || !valid {
				return errCredentialsChanged
			}
		}
		if !canReadQ(tx, u.Name, room) {
			return errors.New("room access denied")
		}
		// Deliberately update only this flag; draft text/version/reading anchors
		// belong to the independent CAS persistence protocol.
		_, err = tx.Exec(`INSERT INTO preferences(user,scope,`+key+`) VALUES(?,?,?) ON CONFLICT(user,scope) DO UPDATE SET `+key+`=excluded.`+key, u.Name, "room:"+room, on)
		return err
	})
}

func guiRoomPreferencesQ(q queryer, actor string) (map[string]guiRoomPreferences, error) {
	rows, err := q.Query(`SELECT r.name,p.pinned,p.subscribed,p.muted,CASE WHEN p.subscribed THEN (SELECT count(*) FROM messages m WHERE m.room=r.name AND m.recipient='' AND m.sender!=? AND m.id>p.last_read) ELSE 0 END FROM preferences p JOIN rooms r ON p.scope='room:'||r.name LEFT JOIN room_meta x ON x.name=r.name WHERE p.user=? AND (COALESCE(x.private,0)=0 OR r.owner=? OR EXISTS(SELECT 1 FROM members v WHERE v.room=r.name AND v.user=?)) ORDER BY r.name`, actor, actor, actor, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]guiRoomPreferences)
	for rows.Next() {
		var name string
		var p guiRoomPreferences
		if err = rows.Scan(&name, &p.Pinned, &p.Subscribed, &p.Muted, &p.Unread); err != nil {
			return nil, err
		}
		result[name] = p
	}
	return result, rows.Err()
}

func guiRoomPreferenceQ(q queryer, actor, room string) (guiRoomPreferences, error) {
	var p guiRoomPreferences
	err := q.QueryRow(`SELECT pinned,subscribed,muted,CASE WHEN subscribed THEN (SELECT count(*) FROM messages m WHERE m.room=? AND m.recipient='' AND m.sender!=? AND m.id>p.last_read) ELSE 0 END FROM preferences p WHERE user=? AND scope=?`, room, actor, actor, "room:"+room).Scan(&p.Pinned, &p.Subscribed, &p.Muted, &p.Unread)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}
