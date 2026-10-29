package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type guiContact struct {
	Name  string `json:"name"`
	Group string `json:"group"`
}
type guiNotice struct {
	ID      int64  `json:"id"`
	Ref     int64  `json:"ref"`
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Body    string `json:"body"`
	Seen    bool   `json:"seen"`
	Command string `json:"command"`
}
type guiCenter struct {
	User     string       `json:"user"`
	Section  string       `json:"section"`
	Contacts []guiContact `json:"contacts"`
	Notices  []guiNotice  `json:"notices"`
	Unread   int          `json:"unread"`
}

func guiCenterSnapshot(s *Store, actor string) (*guiCenter, error) {
	tx, err := s.readDB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u, err := mutationActorQ(tx, actor)
	if err != nil {
		return nil, err
	}
	p := &guiCenter{User: u.Name, Section: "all"}
	groups := []struct {
		group, query string
		args         []any
	}{
		{"requests", `SELECT owner FROM relations WHERE target=? AND kind='request' ORDER BY at,owner`, []any{u.Name}},
		{"friends", `SELECT target FROM relations WHERE owner=? AND kind='friend' ORDER BY target`, []any{u.Name}},
		{"recent", `SELECT CASE WHEN sender=? THEN recipient ELSE sender END peer FROM messages WHERE recipient!='' AND (sender=? OR recipient=?) GROUP BY peer ORDER BY max(id) DESC LIMIT 30`, []any{u.Name, u.Name, u.Name}},
		{"following", `SELECT target FROM relations WHERE owner=? AND kind='follow' ORDER BY target`, []any{u.Name}},
	}
	for _, g := range groups {
		rows, err := tx.Query(g.query, g.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			p.Contacts = append(p.Contacts, guiContact{Name: name, Group: g.group})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(`SELECT id,ref,at,kind,actor,body,seen FROM notifications WHERE user=? ORDER BY id DESC LIMIT 100`, u.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n guiNotice
		if err = rows.Scan(&n.ID, &n.Ref, &n.At, &n.Kind, &n.Actor, &n.Body, &n.Seen); err != nil {
			rows.Close()
			return nil, err
		}
		n.Body = cleanText(n.Body, 80)
		p.Notices = append(p.Notices, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range p.Notices {
		n := &p.Notices[i]
		if blockedQ(tx, u.Name, n.Actor) {
			n.Body = "已屏蔽的来源"
			n.Ref = 0
			n.Command = ""
			continue
		}
		if n.Ref > 0 {
			m, e := messageQ(tx, n.Ref)
			if e == nil && visibleMessageQ(tx, u.Name, m) && m.Recalled == 0 {
				n.Command = fmt.Sprintf("/jump %d", n.Ref)
			} else {
				n.Body = "关联消息已不可访问"
				n.Ref = 0
			}
		} else {
			switch n.Kind {
			case "invite":
				n.Body = "收到房间邀请"
			case "friend":
				n.Body = "收到好友申请"
			case "poke":
				n.Body = "收到戳一戳"
			}
			if _, e := userQ(tx, n.Actor); e == nil && n.Actor != u.Name {
				n.Command = "/profile " + n.Actor
			}
		}
	}
	if err = tx.QueryRow(`SELECT count(*) FROM notifications WHERE user=? AND seen=0`, u.Name).Scan(&p.Unread); err != nil {
		return nil, err
	}
	return p, nil
}
func guiCenterMutation(s *Store, actor, sessionID, command, target string) error {
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
		if command == "/notifyclear" {
			var through int64
			if err = tx.QueryRow(`SELECT COALESCE(MAX(id),0) FROM notifications WHERE user=?`, u.Name).Scan(&through); err != nil {
				return err
			}
			_, err = tx.Exec(`UPDATE notifications SET seen=1 WHERE user=? AND id<=?`, u.Name, through)
			return err
		}
		if command != "/accept" && command != "/decline" {
			return errors.New("unsupported contact action")
		}
		other, err := resolveUserQ(tx, target)
		if err != nil {
			return err
		}
		var count int
		if err = tx.QueryRow(`SELECT count(*) FROM relations WHERE owner=? AND target=? AND kind='request'`, other.Name, u.Name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("no incoming friend request")
		}
		if _, err = tx.Exec(`DELETE FROM relations WHERE owner=? AND target=? AND kind='request'`, other.Name, u.Name); err != nil {
			return err
		}
		if command == "/accept" {
			for _, pair := range [][2]string{{u.Name, other.Name}, {other.Name, u.Name}} {
				if _, err = tx.Exec(`INSERT OR IGNORE INTO relations VALUES(?,?,'friend','',strftime('%s','now'))`, pair[0], pair[1]); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func guiCenterSignature(p *guiCenter) string { b, _ := json.Marshal(p); return string(b) }
func guiCenterEventRelevant(e Event, user string) bool {
	if e.Refresh || e.MoveRoom != "" || e.ChangedID > 0 || e.Notice != "" {
		return true
	}
	if e.Message != nil {
		if e.Message.Recipient != "" || e.Message.ReplyTo > 0 {
			return true
		}
		for _, name := range mentionTargets(e.Message.Body) {
			if name == user {
				return true
			}
		}
	}
	return false
}
