package main

import (
	"database/sql"
	"errors"
	"time"
)

// The native and terminal clients share the same transaction. Target settings,
// blocks, session revocation, cooldown and notification are checked atomically.
func (s *Store) pokeForSession(actor, target, sessionID string) (string, error) {
	var name string
	err := s.writeTransaction(func(tx *sql.Tx) error {
		u, err := mutationActorQ(tx, actor)
		if err != nil {
			return err
		}
		if sessionID != "" {
			if valid, e := sessionCurrentQ(tx, sessionID, u.Name); e != nil || !valid {
				return errCredentialsChanged
			}
		}
		other, err := resolveUserQ(tx, target)
		if err != nil {
			return err
		}
		name = other.Name
		if name == u.Name {
			return errors.New("不能戳自己")
		}
		prefs := settingsForUser(other)
		if blockedQ(tx, name, u.Name) || prefs["poke"] == "off" || prefs["dnd"] == "on" {
			return errors.New("对方当前不接受戳一戳")
		}
		var last int64
		err = tx.QueryRow(`SELECT at FROM poke_limits WHERE actor=? AND target=?`, u.Name, name).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := time.Now().Unix()
		if now-last < 30 {
			return errors.New("同一对象每 30 秒只能戳一次")
		}
		if _, err = tx.Exec(`INSERT INTO poke_limits VALUES(?,?,?) ON CONFLICT(actor,target) DO UPDATE SET at=excluded.at`, u.Name, name, now); err != nil {
			return err
		}
		return notifyQ(tx, name, "poke", u.Name, 0, "戳了戳你")
	})
	return name, err
}
