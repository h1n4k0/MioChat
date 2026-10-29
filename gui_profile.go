package main

import (
	"database/sql"
	"errors"
	"time"
)

type guiProfile struct {
	CanAdmin   bool     `json:"can_admin"`
	Viewer     string   `json:"viewer"`
	Name       string   `json:"name"`
	UID        int64    `json:"uid"`
	Role       string   `json:"role"`
	Bio        string   `json:"bio"`
	Status     string   `json:"status"`
	Avatar     string   `json:"avatar"`
	AvatarKind string   `json:"avatar_kind"`
	Created    string   `json:"created"`
	LastLogin  string   `json:"last_login"`
	Online     bool     `json:"online"`
	Self       bool     `json:"self"`
	Following  bool     `json:"following"`
	Friend     bool     `json:"friend"`
	Requested  bool     `json:"requested"`
	Blocked    bool     `json:"blocked"`
	Note       string   `json:"note"`
	Badges     []string `json:"badges,omitempty"`
}

func guiProfileSnapshot(s *Store, h *Hub, actor, target string) (*guiProfile, error) {
	tx, err := s.readDB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	viewer, err := mutationActorQ(tx, actor)
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = viewer.Name
	}
	u, err := resolveUserQ(tx, target)
	if err != nil {
		return nil, err
	}
	p := &guiProfile{CanAdmin: isAdmin(viewer.Role), Viewer: viewer.Name, Name: u.Name, UID: u.UID, Role: u.Role, Bio: u.Bio, Status: u.Status, Avatar: avatarFor(u), AvatarKind: u.Avatar, Created: formatUnix(u.Created), LastLogin: formatUnix(u.LastLogin), Self: u.Name == viewer.Name}
	if u.LastLogin == 0 {
		p.LastLogin = "尚无登录记录"
	}
	rows, err := tx.Query(`SELECT kind,value FROM relations WHERE owner=? AND target=?`, viewer.Name, u.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, value string
		if err = rows.Scan(&kind, &value); err != nil {
			rows.Close()
			return nil, err
		}
		switch kind {
		case "follow":
			p.Following = true
		case "friend":
			p.Friend = true
		case "request":
			p.Requested = true
		case "block":
			p.Blocked = true
		case "note":
			p.Note = value
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(`SELECT badge FROM badges WHERE user=? ORDER BY at`, u.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var badge string
		if err = rows.Scan(&badge); err != nil {
			rows.Close()
			return nil, err
		}
		p.Badges = append(p.Badges, badge)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Never hold a DB connection while acquiring Hub's lock.
	if err = tx.Rollback(); err != nil {
		return nil, err
	}
	for _, name := range h.Online() {
		if name == u.Name {
			p.Online = true
		}
	}
	return p, nil
}
func guiProfileMutation(s *Store, actor, sessionID, command, target, value string) error {
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
		if command == "/avatar" {
			if target != "" && target != u.Name {
				return errors.New("only own avatar is editable")
			}
			switch value {
			case "camp", "cat", "robot", "planet", "flower", "ident":
			default:
				return errors.New("avatar: camp cat robot planet flower ident")
			}
			return changedOne(tx.Exec(`UPDATE user_meta SET avatar=? WHERE name=?`, value, u.Name))
		}
		if command == "/afk" || command == "/back" {
			if command == "/afk" {
				if value == "" {
					value = "暂时离开"
				}
				value = "AFK · " + value
			} else {
				value = ""
			}
			command = "/status"
		}
		if command == "/bio" || command == "/status" {
			if target != "" && target != u.Name {
				return errors.New("only own profile is editable")
			}
			field := "bio"
			if command == "/status" {
				field = "status"
			}
			return changedOne(tx.Exec(`UPDATE users SET `+field+`=? WHERE name=?`, cleanText(value, 160), u.Name))
		}
		other, err := resolveUserQ(tx, target)
		if err != nil {
			return err
		}
		if other.Name == u.Name {
			return errors.New("cannot change a relationship with self")
		}
		if command == "/unfriend" {
			_, err = tx.Exec(`DELETE FROM relations WHERE kind='friend' AND ((owner=? AND target=?) OR (owner=? AND target=?))`, u.Name, other.Name, other.Name, u.Name)
			return err
		}
		kind := ""
		remove := false
		switch command {
		case "/follow":
			kind = "follow"
		case "/unfollow":
			kind = "follow"
			remove = true
		case "/friend":
			kind = "request"
		case "/ignore":
			kind = "block"
		case "/unignore":
			kind = "block"
			remove = true
		case "/note":
			kind = "note"
		case "/unnote":
			kind = "note"
			remove = true
		default:
			return errors.New("unsupported profile action")
		}
		if remove {
			_, err = tx.Exec(`DELETE FROM relations WHERE owner=? AND target=? AND kind=?`, u.Name, other.Name, kind)
			return err
		}
		if kind == "request" && blockedQ(tx, other.Name, u.Name) {
			return errors.New("recipient does not accept requests")
		}
		if _, err = tx.Exec(`INSERT INTO relations VALUES(?,?,?,?,?) ON CONFLICT(owner,target,kind) DO UPDATE SET value=excluded.value`, u.Name, other.Name, kind, cleanText(value, 100), time.Now().Unix()); err != nil {
			return err
		}
		if kind == "request" {
			return notifyQ(tx, other.Name, "friend", u.Name, 0, "收到好友申请")
		}
		return nil
	})
}
