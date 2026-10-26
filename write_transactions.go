package main

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

func (s *Store) writeTransaction(operation func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = operation(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func resolveUserQ(q queryer, name string) (User, error) {
	if strings.HasPrefix(name, "#") {
		uid, err := strconv.ParseInt(strings.TrimPrefix(name, "#"), 10, 64)
		if err != nil {
			return User{}, errors.New("无效 UID")
		}
		if err = q.QueryRow(`SELECT name FROM user_meta WHERE uid=?`, uid).Scan(&name); err != nil {
			return User{}, errors.New("账户不存在")
		}
	}
	return userQ(q, accountKey(name))
}

// A mutation checks the actor inside the same transaction as its state and audit.
func mutationActorQ(q queryer, name string) (User, error) {
	u, err := userQ(q, accountKey(name))
	if err != nil {
		return User{}, err
	}
	var banned int
	if err = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='user' AND target=? AND revoked=0 AND (expires=0 OR expires>?)`, u.Name, time.Now().Unix()).Scan(&banned); err != nil {
		return User{}, err
	}
	if u.Banned || banned > 0 || u.MustReset {
		return User{}, errors.New("账户当前不能执行管理操作")
	}
	return u, nil
}

func changedOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("数据已变化或操作未完成，请刷新后重试")
	}
	return nil
}
