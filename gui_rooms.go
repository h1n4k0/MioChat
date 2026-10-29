package main

import "errors"

// Structured room cards and member rows are viewer-specific snapshots. Controls
// are hints only: every submitted mutation is rechecked by Store in a transaction.
type guiRoomCard struct {
	Name          string             `json:"name"`
	Topic         string             `json:"topic"`
	Owner         string             `json:"owner"`
	Category      string             `json:"category"`
	Rules         string             `json:"rules"`
	Announcement  string             `json:"announcement"`
	Private       bool               `json:"private"`
	ReadOnly      bool               `json:"read_only"`
	Archived      bool               `json:"archived"`
	Slow          int                `json:"slow"`
	CanOwn        bool               `json:"can_own"`
	CanAssignMods bool               `json:"can_assign_mods"`
	Preferences   guiRoomPreferences `json:"preferences"`
	CanManage     bool               `json:"can_manage"`
	CanLeave      bool               `json:"can_leave"`
}
type guiRoomMember struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	CanRemove bool   `json:"can_remove"`
	CanMod    bool   `json:"can_mod"`
	CanUnmod  bool   `json:"can_unmod"`
}

func guiRoomCardQ(q queryer, actor, room string) (guiRoomCard, error) {
	if !canReadQ(q, actor, room) {
		return guiRoomCard{}, errors.New("room access denied")
	}
	r, err := roomQ(q, room)
	if err != nil {
		return guiRoomCard{}, err
	}
	u, err := userQ(q, actor)
	if err != nil {
		return guiRoomCard{}, err
	}
	prefs, err := guiRoomPreferenceQ(q, actor, room)
	if err != nil {
		return guiRoomCard{}, err
	}
	return guiRoomCard{CanOwn: r.Name != "lobby" && (r.Owner == actor || isAdmin(u.Role)), CanAssignMods: r.Owner == actor || isAdmin(u.Role), Preferences: prefs, Name: r.Name, Topic: r.Topic, Owner: r.Owner, Category: r.Category, Rules: r.Rules, Announcement: r.Announcement, Private: r.Private, ReadOnly: r.ReadOnly, Archived: r.Archived, Slow: r.Slow, CanManage: canManageQ(q, actor, room), CanLeave: r.Owner != actor}, nil
}
func guiRoomDirectory(s *Store, actor string, archived bool) (*guiPanel, error) {
	tx, err := s.readDB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rooms, err := visibleRoomsQ(tx, actor, archived)
	if err != nil {
		return nil, err
	}
	p := &guiPanel{Kind: "rooms", Title: "房间目录"}
	for _, r := range rooms {
		card, err := guiRoomCardQ(tx, actor, r.Name)
		if err != nil {
			return nil, err
		}
		p.Rooms = append(p.Rooms, card)
		p.Actions = append(p.Actions, guiPanelAction{Label: "#" + r.Name, Command: "/join " + r.Name})
	}
	return p, nil
}
func guiRoomPeople(s *Store, actor, room string, members bool) (*guiPanel, error) {
	tx, err := s.readDB().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	card, err := guiRoomCardQ(tx, actor, room)
	if err != nil {
		return nil, err
	}
	p := &guiPanel{Kind: "roominfo", Scope: room, Title: "#" + room, Details: &card}
	if members {
		p.Kind = "members"
		rows, err := tx.Query(`SELECT user,role FROM members WHERE room=? ORDER BY role,user`, room)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var member guiRoomMember
			if err = rows.Scan(&member.Name, &member.Role); err != nil {
				rows.Close()
				return nil, err
			}
			member.CanRemove = card.CanManage && member.Name != card.Owner
			member.CanMod = card.CanAssignMods && member.Name != card.Owner && member.Role != "moderator"
			member.CanUnmod = card.CanAssignMods && member.Name != card.Owner && member.Role == "moderator"
			p.Members = append(p.Members, member)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, member := range p.Members {
			if member.CanRemove {
				p.Actions = append(p.Actions, guiPanelAction{Label: "移除 @" + member.Name, Command: "/remove " + member.Name})
			}
		}
	}
	if members {
		p.Actions = append(p.Actions, guiPanelAction{Label: "房间资料", Command: "/roominfo"})
	} else {
		p.Actions = append(p.Actions, guiPanelAction{Label: "成员", Command: "/members"})
	}
	if card.CanManage {
		p.Actions = append(p.Actions, guiPanelAction{Label: "邀请成员", Command: "/invite ", Fill: true})
	}
	for _, item := range []struct {
		label, command string
		on             bool
	}{{"房间置顶", "/roompin", card.Preferences.Pinned}, {"订阅未读", "/subscribe", card.Preferences.Subscribed}, {"房间静音", "/roommute", card.Preferences.Muted}} {
		value := "on"
		label := item.label
		if item.on {
			value = "off"
			label = "取消" + label
		}
		p.Actions = append(p.Actions, guiPanelAction{Label: label, Command: item.command + " " + value})
	}
	if card.CanManage {
		for _, key := range []string{"category", "rules", "announcement", "slow"} {
			p.Actions = append(p.Actions, guiPanelAction{Label: "编辑 " + key, Command: "/roomset " + key + " ", Fill: true})
		}
		value := "on"
		if card.ReadOnly {
			value = "off"
		}
		p.Actions = append(p.Actions, guiPanelAction{Label: "切换只读", Command: "/roomset readonly " + value})
	}
	if card.CanOwn {
		value := "on"
		if card.Private {
			value = "off"
		}
		p.Actions = append(p.Actions, guiPanelAction{Label: "切换私密", Command: "/roomset private " + value}, guiPanelAction{Label: "转让房主", Command: "/roomset transfer ", Fill: true})
		command := "/archive"
		label := "归档房间"
		if card.Archived {
			command = "/unarchive"
			label = "恢复房间"
		}
		p.Actions = append(p.Actions, guiPanelAction{Label: label, Command: command})
	}
	if card.CanLeave {
		p.Actions = append(p.Actions, guiPanelAction{Label: "离开房间", Command: "/leave"})
	}
	return p, nil
}
