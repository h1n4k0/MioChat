package main

import (
	"context"
	"time"
)

func (s *Store) SessionHealth(c *Client) (User, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var u User
	var valid, banned bool
	err := s.policyDB().QueryRowContext(ctx, `SELECT u.name,u.bio,u.status,u.theme,u.role,u.created,u.banned,x.uid,x.last_login,x.avatar,x.must_reset,x.settings,COALESCE(v.epoch,0),
EXISTS(SELECT 1 FROM sessions s LEFT JOIN session_versions z ON z.session_id=s.id WHERE s.id=? AND s.user=u.name AND s.closed=0 AND s.revoked=0 AND COALESCE(z.epoch,0)=COALESCE(v.epoch,0) AND NOT EXISTS(SELECT 1 FROM session_credentials k LEFT JOIN ssh_keys p ON p.id=k.key_id WHERE k.session_id=s.id AND (p.id IS NULL OR p.user!=s.user OR p.revoked!=0))),
EXISTS(SELECT 1 FROM sanctions a WHERE a.kind='user' AND a.target=u.name AND a.revoked=0 AND (a.expires=0 OR a.expires>?))
FROM users u JOIN user_meta x ON x.name=u.name LEFT JOIN credential_versions v ON v.user=u.name WHERE u.name=?`, c.ID, time.Now().Unix(), c.Name).Scan(&u.Name, &u.Bio, &u.Status, &u.Theme, &u.Role, &u.Created, &u.Banned, &u.UID, &u.LastLogin, &u.Avatar, &u.MustReset, &u.Settings, &u.AuthEpoch, &valid, &banned)
	if err != nil || !valid || banned || u.Banned || s.IPBannedContext(ctx, c.IP) {
		return User{}, false
	}
	return u, true
}

func (s *Store) TouchSession(id string) {
	now := time.Now().Unix()
	_, _ = s.db.Exec(`UPDATE sessions SET seen=? WHERE id=? AND closed=0 AND revoked=0 AND seen<=?`, now, id, now-60)
}
