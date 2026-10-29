package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type guiLoginRow struct {
	IP     string `json:"ip"`
	Client string `json:"client"`
	OK     bool   `json:"ok"`
	At     int64  `json:"at"`
	Time   string `json:"time"`
}
type guiIPAccount struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}
type guiDiagnosticRow struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`
	Time   string `json:"time"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}
type guiModerationView struct {
	Diagnostics []guiDiagnosticRow  `json:"diagnostics,omitempty"`
	Health      *guiStatistics      `json:"health,omitempty"`
	Sanctions   []guiSanctionRow    `json:"sanctions,omitempty"`
	Audits      []guiAuditRow       `json:"audits,omitempty"`
	Recalls     []guiRecallRow      `json:"recalls,omitempty"`
	Reports     []guiReportRow      `json:"reports,omitempty"`
	Reported    *guiReportedMessage `json:"reported,omitempty"`
	Revisions   *guiRevisionPage    `json:"revisions,omitempty"`
	Viewer      string              `json:"viewer"`
	Command     string              `json:"command"`
	Requested   string              `json:"requested"`
	Canonical   string              `json:"canonical"`
	OriginScope string              `json:"origin_scope"`
	Profile     *guiProfile         `json:"profile,omitempty"`
	Logins      []guiLoginRow       `json:"logins,omitempty"`
	Users       []guiIPAccount      `json:"users,omitempty"`
	Window      int                 `json:"window"`
	Explanation string              `json:"explanation"`
	Audited     bool                `json:"audited"`
}

func guiWhoisProjectionQ(q queryer, viewer User, target string) (*guiProfile, error) {
	u, e := resolveUserQ(q, target)
	if e != nil {
		return nil, e
	}
	p := &guiProfile{Viewer: viewer.Name, Name: u.Name, UID: u.UID, Role: u.Role, Bio: u.Bio, Status: u.Status, Avatar: avatarFor(u), AvatarKind: u.Avatar, Created: formatUnix(u.Created), LastLogin: formatUnix(u.LastLogin), Self: u.Name == viewer.Name}
	if u.LastLogin == 0 {
		p.LastLogin = "尚无登录记录"
	}
	e = q.QueryRow(`SELECT value FROM relations WHERE owner=? AND target=? AND kind='note'`, viewer.Name, u.Name).Scan(&p.Note)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	rows, e := q.Query(`SELECT badge FROM badges WHERE user=? ORDER BY at`, u.Name)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var badge string
		if e = rows.Scan(&badge); e != nil {
			return nil, e
		}
		p.Badges = append(p.Badges, badge)
	}
	return p, rows.Err()
}
func guiModerationDataQ(q queryer, viewer User, command, arg, scope string, request guiRequest) (*guiModerationView, error) {
	view := &guiModerationView{Viewer: viewer.Name, Command: command, Requested: arg, OriginScope: scope}
	switch command {
	case "/errors", "/health":
		if !isAdmin(viewer.Role) {
			return nil, errors.New("需要管理员权限")
		}
		if arg != "" {
			return nil, errors.New("此查询无需参数")
		}
		if command == "/health" {
			var e error
			view.Health, e = guiStatisticsDataQ(q, time.Now())
			if e != nil {
				return nil, e
			}
			view.Explanation = "管理员运行健康快照；数据库读取失败不会显示为零。运行时与磁盘采样不属于300毫秒数据库预算。"
		} else {
			view.Window = 100
			view.Explanation = "最近最多100条诊断原文，仅当前授权管理员可见。原文可能含输入或连接信息，并非已脱敏；不会自动复制或写入聊天、草稿。"
			rows, e := q.Query(`SELECT id,at,kind,detail FROM diagnostics ORDER BY id DESC LIMIT 100`)
			if e != nil {
				return nil, e
			}
			defer rows.Close()
			for rows.Next() {
				var row guiDiagnosticRow
				if e = rows.Scan(&row.ID, &row.At, &row.Kind, &row.Detail); e != nil {
					return nil, e
				}
				row.Time = time.Unix(row.At, 0).UTC().Format(time.RFC3339)
				view.Diagnostics = append(view.Diagnostics, row)
			}
			if e = rows.Err(); e != nil {
				return nil, e
			}
		}
	case "/admin":
		if !isAdmin(viewer.Role) {
			return nil, errors.New("需要管理员权限")
		}
		view.Explanation = "管理导航；每个操作仍单独核验当前权限。未移植功能明确标注，不会代为执行。"
	case "/whois":
		if !isAdmin(viewer.Role) {
			return nil, errors.New("需要管理员权限")
		}
		p, e := guiWhoisProjectionQ(q, viewer, arg)
		if e != nil {
			return nil, e
		}
		view.Profile = p
		view.Canonical = p.Name
		view.Explanation = "只展示原用户卡公开字段及查看者自己的私有备注；不包含凭据或账户内部设置。"
	case "/ip", "/logins":
		target := viewer.Name
		if arg != "" {
			u, e := resolveUserQ(q, arg)
			if e != nil {
				return nil, e
			}
			target = u.Name
		}
		if target != viewer.Name && !isAdmin(viewer.Role) {
			return nil, errors.New("只有管理员可查看他人登录 IP")
		}
		view.Canonical = target
		view.Window = 100
		view.Explanation = "最近最多100条登录记录，包含成功与失败；不是全部历史。仅有权查看者可见。"
		rows, e := q.Query(`SELECT ip,client,ok,at FROM logins WHERE user=? ORDER BY id DESC LIMIT 100`, target)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		for rows.Next() {
			var row guiLoginRow
			if e = rows.Scan(&row.IP, &row.Client, &row.OK, &row.At); e != nil {
				return nil, e
			}
			row.Time = formatUnix(row.At)
			view.Logins = append(view.Logins, row)
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
	case "/ipusers":
		if !isAdmin(viewer.Role) {
			return nil, errors.New("需要管理员权限")
		}
		addr, e := netip.ParseAddr(arg)
		if e != nil {
			return nil, errors.New("请输入一个有效 IP（不是CIDR）")
		}
		view.Canonical = addr.Unmap().String()
		view.Window = 200
		view.Explanation = "同IP成功登录且仍存在的账户，按姓名排序最多200个。共享网络仅作线索，不证明是同一人；匹配原输入及Unmap规范形式，不宣称完整历史IP等价归并。"
		rows, e := q.Query(`SELECT DISTINCT l.user,COALESCE(r.value,'') FROM logins l JOIN users u ON u.name=l.user LEFT JOIN relations r ON r.owner=? AND r.target=l.user AND r.kind='note' WHERE l.ip IN (?,?) AND l.ok=1 ORDER BY l.user LIMIT 200`, viewer.Name, arg, view.Canonical)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var row guiIPAccount
			if e = rows.Scan(&row.Name, &row.Note); e != nil {
				rows.Close()
				return nil, e
			}
			view.Users = append(view.Users, row)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if e = auditQ(q, viewer.Name, "ip.lookup", view.Canonical, "共享地址仅作线索", ""); e != nil {
			return nil, e
		}
		view.Audited = true
	default:
		if e := guiModerationContentQ(q, viewer, view, request); e != nil {
			return nil, e
		}
	}
	return view, nil
}
func guiModerationRead(store *Store, hub *Hub, user User, scope guiScope, request guiRequest, command, arg string) (*guiPanel, error) {
	if request.ctx == nil || strings.TrimSpace(request.sessionID) == "" {
		return nil, errCredentialsChanged
	}
	ctx, cancel := context.WithTimeout(request.ctx, 300*time.Millisecond)
	defer cancel()
	var view *guiModerationView
	if command == "/ipusers" || command == "/reportview" || command == "/revisions" {
		e := store.communityWriteContext(ctx, user.Name, request.sessionID, func(q queryer) error {
			viewer, e := mutationActorQ(q, user.Name)
			if e != nil {
				return e
			}
			view, e = guiModerationDataQ(q, viewer, command, arg, scope.key(), request)
			return e
		})
		if e != nil {
			return nil, e
		} // No result escapes an uncommitted/failed audit read.
	} else {
		tx, e := store.readDB().BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		q := scopedQuery{ctx: ctx, db: tx}
		if valid, e := sessionCurrentQ(q, request.sessionID, user.Name); e != nil || !valid {
			return nil, errCredentialsChanged
		}
		viewer, e := mutationActorQ(q, user.Name)
		if e != nil {
			return nil, e
		}
		view, e = guiModerationDataQ(q, viewer, command, arg, scope.key(), request)
		if e != nil {
			return nil, e
		}
		if e = tx.Rollback(); e != nil {
			return nil, e
		}
	}
	if view.Health != nil {
		guiStatisticsRuntime(store, hub, view.Health, time.Now())
	}
	// Hub lock is taken only after releasing the database connection.
	if view.Profile != nil {
		for _, name := range hub.Online() {
			if name == view.Profile.Name {
				view.Profile.Online = true
				break
			}
		}
	}
	panel := &guiPanel{Kind: "moderation", Scope: scope.key(), Title: "管理查询", Moderation: view}
	switch command {
	case "/admin":
		panel.Title = "管理控制台"
		for _, entry := range commandCatalog {
			if !entry.Admin {
				continue
			}
			if guiImplementedCommand(entry.Name) {
				panel.Actions = append(panel.Actions, guiPanelAction{Label: entry.Description + "  " + entry.Name, Command: entry.Name + " ", Fill: entry.Args != ""})
			} else {
				panel.Lines = append(panel.Lines, entry.Name+" · "+entry.Description+"（尚未移植，未执行）")
			}
		}
	case "/whois":
		panel.Title = "账户详情 · @" + view.Canonical
		panel.Actions = []guiPanelAction{{Label: "打开原生资料与关系操作", Command: "/profile " + view.Canonical}, {Label: "查看登录记录", Command: "/ip " + view.Canonical}, {Label: "查看连接会话", Command: "/sessions " + view.Canonical}}
	case "/ip", "/logins":
		panel.Title = "@" + view.Canonical + " · 登录记录"
	case "/ipusers":
		panel.Title = "同IP账户 · 仅作线索"
		for _, row := range view.Users {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "资料 @" + row.Name, Command: "/profile " + row.Name})
		}
	}

	switch command {
	case "/errors":
		panel.Title = "管理员诊断 · 原文未脱敏"
	case "/health":
		panel.Title = "管理员运行健康"
	case "/bans":
		panel.Title = "封禁与禁言记录"
	case "/audit":
		panel.Title = "管理员操作审计"
	case "/recalls":
		panel.Title = "公开频道撤回目录"
		for _, r := range view.Recalls {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: fmt.Sprintf("核验修订权限 · 消息 #%d", r.ID), Command: fmt.Sprintf("/revisions %d", r.ID)})
		}
	case "/reports":
		panel.Title = "举报队列"
		for _, r := range view.Reports {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: fmt.Sprintf("审计读取举报 #%d", r.ID), Command: fmt.Sprintf("/reportview %d", r.ID)})
		}
	case "/reportview":
		panel.Title = "举报消息 · 单条例外"
		for _, status := range []string{"resolved", "rejected"} {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "设置处理状态为 " + status + "（需确认）", Command: fmt.Sprintf("/resolve %d %s", view.Reported.ReportID, status)})
		}
	case "/revisions":
		panel.Title = "授权修订与撤回原文"
		if view.Revisions.NextAfter > 0 {
			panel.Actions = append(panel.Actions, guiPanelAction{Label: "下一页修订（仍需授权）", Command: "local:revisions-next"})
		}
	}
	if command != "/errors" && command != "/health" {
		panel.Actions = append(panel.Actions, guiPanelAction{Label: "刷新此查询", Command: strings.TrimSpace(command + " " + arg)})
	}
	return panel, nil
}
