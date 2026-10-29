package main

import (
	"database/sql"
	"errors"
	"net/netip"
	"time"
)

type sanctionSpec struct {
	kind, target, room, reason string
	expires                    int64
	remove                     bool
}

func prepareSanction(kind, target, room, duration, reason string, remove bool) (sanctionSpec, error) {
	if kind != "ip" && kind != "user" && kind != "mute" {
		return sanctionSpec{}, errors.New("未知封禁类型")
	}
	if kind == "ip" {
		p, e := normalizePrefix(target)
		if e != nil {
			return sanctionSpec{}, e
		}
		if p.Bits() == 0 {
			return sanctionSpec{}, errors.New("不能封禁整个互联网地址段")
		}
		target = p.String()
	}
	expires := int64(0)
	if !remove {
		seconds, e := durationSeconds(duration)
		if e != nil {
			return sanctionSpec{}, e
		}
		if seconds > 0 {
			expires = time.Now().Unix() + seconds
		}
	}

	return sanctionSpec{kind: kind, target: target, room: room, reason: reason, expires: expires, remove: remove}, nil
}
func sanctionQ(q queryer, actor string, p *sanctionSpec) error {
	kind, target, room, reason, expires, remove := p.kind, p.target, p.room, p.reason, p.expires, p.remove
	u, e := mutationActorQ(q, actor)
	if e != nil {
		return e
	}
	global := isAdmin(u.Role)
	if !global && (kind != "mute" || room == "" || !canManageQ(q, u.Name, room)) {
		return errors.New("没有此管理权限")
	}
	if kind != "ip" {
		other, e := resolveUserQ(q, target)
		if e != nil {
			return e
		}
		target = other.Name
		if target == u.Name || roleRank(other.Role) >= roleRank(u.Role) && global {
			return errors.New("不能管理自己、同级或更高角色")
		}
		if !global {
			r, e := roomQ(q, room)
			if e != nil {
				return e
			}
			if other.Name == r.Owner || isAdmin(other.Role) {
				return errors.New("不能禁言频道所有者或管理员")
			}
		}
	}
	if remove {
		if kind == "ip" {
			desired, e := normalizePrefix(target)
			if e != nil {
				return e
			}
			rows, e := q.Query(`SELECT id,target FROM sanctions WHERE kind='ip' AND room=? AND revoked=0`, room)
			if e != nil {
				return e
			}
			var matches []int64
			for rows.Next() {
				var id int64
				var stored string
				if e = rows.Scan(&id, &stored); e != nil {
					rows.Close()
					return e
				}
				prefix, err := normalizePrefix(stored)
				if err != nil {
					// Legacy wide mapped prefixes remain IPv6 policies; they
					// must never be promoted into a blanket IPv4 /0 policy.
					if legacy, parseErr := netip.ParsePrefix(stored); parseErr == nil && legacy.Addr().Is4In6() && legacy.Bits() < 96 {
						prefix, err = legacy.Masked(), nil
					}
				}
				if err == nil && prefix == desired {
					matches = append(matches, id)
				}
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			for _, id := range matches {
				if e = changedOne(q.Exec(`UPDATE sanctions SET revoked=1 WHERE id=? AND kind='ip'`, id)); e != nil {
					return e
				}
			}
		} else {
			// Include expired but unrevoked records, as the original removal
			// did. Zero matches is a legitimate audited idempotent request.
			var expected int64
			if e = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind=? AND target=? AND room=? AND revoked=0`, kind, target, room).Scan(&expected); e != nil {
				return e
			}
			updated, e := q.Exec(`UPDATE sanctions SET revoked=1 WHERE kind=? AND target=? AND room=? AND revoked=0`, kind, target, room)
			if e != nil {
				return e
			}
			changed, e := updated.RowsAffected()
			if e != nil {
				return e
			}
			if changed != expected {
				return errors.New("解除限制未完整执行，未提交任何修改")
			}
			var remaining int64
			if e = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind=? AND target=? AND room=? AND revoked=0`, kind, target, room).Scan(&remaining); e != nil {
				return e
			}
			if remaining != 0 {
				return errors.New("解除限制结果未确认，未提交任何修改")
			}
		}
		if kind == "user" {
			if e = changedOne(q.Exec(`UPDATE users SET banned=0 WHERE name=?`, target)); e != nil {
				return e
			}
		}
	} else if e = changedOne(q.Exec(`INSERT INTO sanctions(kind,target,room,expires,reason,actor,at) VALUES(?,?,?,?,?,?,?)`, kind, target, room, expires, cleanText(reason, 240), u.Name, time.Now().Unix())); e != nil {
		return e
	}
	action := "sanction.add"
	if remove {
		action = "sanction.remove"
	}
	p.target = target
	return auditQ(q, u.Name, action, kind+":"+target, reason, room)
}
func (s *Store) Sanction(actor, kind, target, room, duration, reason string, remove bool) error {
	p, e := prepareSanction(kind, target, room, duration, reason, remove)
	if e != nil {
		return e
	}
	e = s.writeTransaction(func(tx *sql.Tx) error { return sanctionQ(tx, actor, &p) })
	if e == nil && kind == "ip" {
		s.invalidateIPPolicy()
	}
	return e
}
