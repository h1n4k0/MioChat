package main

import "fmt"

// These controls are hints, never authorization: every command retains its
// server-side current visibility and mutation checks when invoked.
func guiMessageContext(s *Store, u User, scope guiScope, m Message) *guiPanel {
	id := m.ID
	actions := []guiPanelAction{{Label: "定位消息", Command: fmt.Sprintf("/jump %d", id)}, {Label: "回复", Command: fmt.Sprintf("/reply %d", id)}, {Label: "收藏", Command: fmt.Sprintf("/bookmark %d", id)}, {Label: "取消收藏", Command: fmt.Sprintf("/unbookmark %d", id)}, {Label: "举报此条（确认）", Command: fmt.Sprintf("/report %d", id)}}
	for _, reaction := range []string{"like", "laugh", "question", "heart", "wow", "sad"} {
		actions = append(actions, guiPanelAction{Label: "回应 " + reaction, Command: fmt.Sprintf("/react %d %s", id, reaction)})
	}
	if m.Recalled == 0 && m.Sender == u.Name {
		actions = append(actions, guiPanelAction{Label: "编辑原文（5分钟内）", Command: fmt.Sprintf("/edit %d", id)}, guiPanelAction{Label: "撤回（5分钟内）", Command: fmt.Sprintf("/recall %d", id)})
	}
	if m.Recipient == "" && scope.room == m.Room && s.canManage(u, m.Room) {
		if m.Recalled == 0 && m.Sender != u.Name {
			actions = append(actions, guiPanelAction{Label: "管理员撤回", Command: fmt.Sprintf("/recall %d", id)})
		}
		actions = append(actions, guiPanelAction{Label: "置顶", Command: fmt.Sprintf("/pin %d", id)})
	}
	return &guiPanel{Kind: "message", Scope: scope.key(), Title: fmt.Sprintf("消息 #%d · 展开阅读", id), Lines: []string{"@" + m.Sender, m.Body}, Actions: actions}
}
