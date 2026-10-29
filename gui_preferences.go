package main

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type guiPreferences struct {
	Rooms    map[string]guiRoomPreferences `json:"rooms,omitempty"`
	Blocked  []string                      `json:"blocked,omitempty"`
	User     string                        `json:"user"`
	Theme    string                        `json:"theme"`
	Settings map[string]string             `json:"settings"`
	KeyCount int                           `json:"key_count"`
}

func guiPreferencesSnapshot(s *Store, actor string) (*guiPreferences, error) {
	tx, err := s.readDB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := userQ(tx, actor)
	if err != nil {
		return nil, err
	}
	if u.MustReset {
		return nil, errors.New("password reset required")
	}
	// Store preserves extension keys for the original TUI. Native controls
	// receive only supported booleans; unknown values are never overwritten.
	settings := settingsForUser(User{})
	saved := settingsForUser(u)
	for key := range settings {
		if value := saved[key]; value == "on" || value == "off" {
			settings[key] = value
		}
	}
	p := &guiPreferences{User: u.Name, Theme: u.Theme, Settings: settings}
	rows, err := tx.Query(`SELECT target FROM relations WHERE owner=? AND kind='block' ORDER BY target`, u.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		p.Blocked = append(p.Blocked, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.QueryRow(`SELECT count(*) FROM ssh_keys WHERE user=? AND revoked=0`, u.Name).Scan(&p.KeyCount); err != nil {
		return nil, err
	}
	p.Rooms, err = guiRoomPreferencesQ(tx, u.Name)
	if err != nil {
		return nil, err
	}
	return p, nil
}
func (p *guiPreferences) reminder() bool {
	return p != nil && p.KeyCount == 0 && p.Settings["key_reminder"] != "off"
}
func guiSetTheme(s *Store, actor, theme, sessionID string) error {
	switch theme {
	case "aurora", "sakura", "amber", "mono", "light", "contrast":
	default:
		return errors.New("theme: aurora sakura amber mono light contrast")
	}
	return s.writeTransaction(func(tx *sql.Tx) error {
		u, err := mutationActorQ(tx, actor)
		if err != nil {
			return err
		}
		if sessionID != "" {
			if valid, err := sessionCurrentQ(tx, sessionID, u.Name); err != nil || !valid {
				return errCredentialsChanged
			}
		}
		return changedOne(tx.Exec(`UPDATE users SET theme=? WHERE name=?`, theme, u.Name))
	})
}
func guiPreferenceSignature(p *guiPreferences) string { b, _ := json.Marshal(p); return string(b) }
