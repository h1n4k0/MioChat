package main

import (
	"database/sql"
	"errors"
	"strconv"
)

type configurationChange struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func configurationValue(key, value string) (string, error) {
	switch key {
	case "registration":
		if value != "open" && value != "closed" && value != "invite" {
			return "", errors.New("registration: open / closed / invite")
		}
	case "registration_quota":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return "", errors.New("注册配额范围 1–100")
		}
	case "maintenance":
		if value != "on" && value != "off" {
			return "", errors.New("maintenance: on / off")
		}
	case "maintenance_notice":
		value = cleanText(value, 240)
	case "retention":
		if value != "permanent" {
			return "", errors.New("消息必须永久保留，禁止启用自动清理")
		}
	default:
		return "", errors.New("未知配置项")
	}
	return value, nil
}

// All settings, fresh authority and audit records belong to the same writer.
// The returned values were read back inside that transaction, not guessed from
// the requested notice (whose cleanText normalization is server-authoritative).
func configurationChangesQ(q queryer, actor string, changes []configurationChange) ([]configurationChange, error) {
	if len(changes) == 0 || len(changes) > 2 {
		return nil, errors.New("无效配置变更")
	}
	normalized := make([]configurationChange, 0, len(changes))
	seen := make(map[string]bool)
	for _, change := range changes {
		if seen[change.Key] {
			return nil, errors.New("重复配置项")
		}
		seen[change.Key] = true
		value, err := configurationValue(change.Key, change.Value)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, configurationChange{Key: change.Key, Value: value})
	}
	u, err := mutationActorQ(q, actor)
	if err != nil {
		return nil, err
	}
	if !isAdmin(u.Role) {
		return nil, errors.New("需要管理员权限")
	}
	for _, change := range normalized {
		if err = changedOne(q.Exec(`INSERT INTO config VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, change.Key, change.Value)); err != nil {
			return nil, err
		}
		if err = auditQ(q, u.Name, "config.set", change.Key, change.Value, ""); err != nil {
			return nil, err
		}
	}
	// Recheck the final set after every write and audit: a later statement must
	// not silently invalidate an earlier setting while returning success.
	for _, change := range normalized {
		var stored string
		if err = q.QueryRow(`SELECT value FROM config WHERE key=?`, change.Key).Scan(&stored); err != nil {
			return nil, err
		}
		if stored != change.Value {
			return nil, errors.New("配置未按请求保存")
		}
	}
	return normalized, nil
}

func maintenanceChanges(state, notice string) []configurationChange {
	changes := []configurationChange{{Key: "maintenance", Value: state}}
	if notice != "" {
		changes = append(changes, configurationChange{Key: "maintenance_notice", Value: notice})
	}
	return changes
}

// Omitted notice preserves the previous value; /config maintenance_notice is
// the explicit clear path. A failing second write/audit rolls back both changes.
func (s *Store) SetMaintenance(actor, state, notice string) error {
	return s.writeTransaction(func(tx *sql.Tx) error {
		_, err := configurationChangesQ(tx, actor, maintenanceChanges(state, notice))
		return err
	})
}
