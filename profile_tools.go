package main

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func avatarFor(user User) string {
	if user.Avatar != "ident" {
		return avatarArt(user.Avatar)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", user.UID, user.Name)))
	var rows []string
	for y := 0; y < 5; y++ {
		var line strings.Builder
		for x := 0; x < 5; x++ {
			column := x
			if x > 2 {
				column = 4 - x
			}
			if sum[y*3+column]&1 != 0 {
				line.WriteString("##")
			} else {
				line.WriteString("  ")
			}
		}
		rows = append(rows, line.String())
	}
	return strings.Join(rows, "\n")
}
func (s *Store) IPUsers(actor, ip string) ([]string, error) {
	u, e := s.User(actor)
	if e != nil || !isAdmin(u.Role) {
		return nil, fmt.Errorf("需要管理员权限")
	}
	addr, e := netip.ParseAddr(ip)
	if e != nil {
		return nil, fmt.Errorf("请输入一个有效 IP")
	}
	rows, e := s.db.Query(`SELECT DISTINCT l.user FROM logins l JOIN users u ON u.name=l.user WHERE l.ip IN (?,?) AND l.ok=1 ORDER BY l.user LIMIT 200`, ip, addr.Unmap().String())
	if e != nil {
		return nil, e
	}
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, name)
		}
	}
	rows.Close()
	_ = auditQ(s.db, actor, "ip.lookup", addr.Unmap().String(), "共享地址仅作线索", "")
	return names, nil
}
func (segment messageSegment) nameAt(line, column, width int) bool {
	y, x := segment.NameLine, segment.NameX
	for _, r := range segment.User {
		w := ansi.StringWidth(string(r))
		if x+w > width {
			y++
			x = 0
		}
		if line == y && column >= x && column < x+w {
			return true
		}
		x += w
	}
	return false
}
