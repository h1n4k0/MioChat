package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Secrets are generated once and returned only to the immediate caller. They
// are never retrievable from storage, logged, or retained for a retry. Clearing
// a Go string is not a promise of cryptographic memory erasure.
type secretAdminOutcome struct {
	kind, actor, target, secret string
	epoch, expires, targetEpoch int64
	uses                        int
	committed                   bool
}

func (o secretAdminOutcome) String() string {
	return fmt.Sprintf("secretAdminOutcome{kind:%s committed:%t}", o.kind, o.committed)
}
func (o secretAdminOutcome) GoString() string { return o.String() }

type secretAdminCommitError struct{}

func (secretAdminCommitError) Error() string {
	return "管理凭据操作提交结果待确认；可能已经生效，不会自动重试，请先核验原服务器"
}

func secretAdminActorQ(q queryer, actor, sessionID string) (User, error) {
	u, err := mutationActorQ(q, actor)
	if err != nil {
		return User{}, err
	}
	if !isAdmin(u.Role) {
		return User{}, errors.New("需要管理员权限")
	}
	if sessionID != "" {
		valid, e := sessionCurrentQ(q, sessionID, u.Name)
		if e != nil {
			return User{}, e
		}
		if !valid {
			return User{}, errCredentialsChanged
		}
	}
	return u, nil
}

func (s *Store) InviteCodeForSession(ctx context.Context, actor, duration string, uses int, sessionID string) (secretAdminOutcome, error) {
	if ctx == nil || strings.TrimSpace(sessionID) == "" {
		return secretAdminOutcome{}, errCredentialsChanged
	}
	return s.inviteCodeForSession(ctx, actor, duration, uses, sessionID)
}
func (s *Store) inviteCodeForSession(ctx context.Context, actor, duration string, uses int, sessionID string) (secretAdminOutcome, error) {
	if err := ctx.Err(); err != nil {
		return secretAdminOutcome{}, err
	}
	if uses < 1 || uses > 100 {
		return secretAdminOutcome{}, errors.New("使用次数范围 1–100")
	}
	seconds, err := durationSeconds(duration)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	token, err := secretToken()
	if err != nil {
		return secretAdminOutcome{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	u, err := secretAdminActorQ(q, actor, sessionID)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	now, expires := time.Now().Unix(), int64(0)
	if seconds > 0 {
		expires = now + seconds
	}
	hash := tokenHash(token)
	if err = changedOne(q.Exec(`INSERT INTO invites VALUES(?,?,?,?,?)`, hash, u.Name, expires, uses, now)); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = auditQ(q, u.Name, "invite.create", "", "次数 "+strconv.Itoa(uses), ""); err != nil {
		return secretAdminOutcome{}, err
	}
	var storedActor string
	var storedExpires int64
	var storedUses int
	if err = q.QueryRow(`SELECT creator,expires,remaining FROM invites WHERE hash=?`, hash).Scan(&storedActor, &storedExpires, &storedUses); err != nil {
		return secretAdminOutcome{}, err
	}
	if storedActor != u.Name || storedExpires != expires || storedUses != uses {
		return secretAdminOutcome{}, errors.New("邀请码写入未完成，操作已回滚")
	}
	if err = ctx.Err(); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = tx.Commit(); err != nil {
		return secretAdminOutcome{}, secretAdminCommitError{}
	}
	return secretAdminOutcome{kind: "invitecode", actor: u.Name, secret: token, epoch: u.AuthEpoch, expires: expires, uses: uses, committed: true}, nil
}

func (s *Store) ResetPasswordForSession(ctx context.Context, actor, target, sessionID string) (secretAdminOutcome, error) {
	if ctx == nil || strings.TrimSpace(sessionID) == "" {
		return secretAdminOutcome{}, errCredentialsChanged
	}
	return s.resetPasswordForSession(ctx, actor, target, sessionID)
}
func (s *Store) resetPasswordForSession(ctx context.Context, actor, target, sessionID string) (secretAdminOutcome, error) {
	if err := ctx.Err(); err != nil {
		return secretAdminOutcome{}, err
	}
	// Bounded expensive work holds neither a connection nor the writer. All
	// authority and target identity are resolved afresh inside the transaction.
	password, err := secretToken()
	if err != nil {
		return secretAdminOutcome{}, err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return secretAdminOutcome{}, err
	}
	hash := passwordHash(password, salt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	u, err := secretAdminActorQ(q, actor, sessionID)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	other, err := resolveUserQ(q, target)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	if other.Name == u.Name || roleRank(other.Role) >= roleRank(u.Role) {
		return secretAdminOutcome{}, errors.New("不能重置此账户的密码")
	}
	if other.AuthEpoch < 0 || other.AuthEpoch == 1<<63-1 {
		return secretAdminOutcome{}, errCredentialsChanged
	}
	if err = credentialWrite(q.Exec(`UPDATE users SET hash=?,salt=? WHERE name=?`, hash, salt, other.Name)); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = setCredentialEpochQ(q, other.Name, other.AuthEpoch+1); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = credentialWrite(q.Exec(`UPDATE user_meta SET must_reset=1 WHERE name=?`, other.Name)); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = revokeSecretTargetQ(q, other.Name); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = auditQ(q, u.Name, "password.reset", other.Name, "临时密码需首次登录修改", ""); err != nil {
		return secretAdminOutcome{}, err
	}
	var storedHash, storedSalt []byte
	if err = q.QueryRow(`SELECT hash,salt FROM users WHERE name=?`, other.Name).Scan(&storedHash, &storedSalt); err != nil {
		return secretAdminOutcome{}, err
	}
	stored, err := userQ(q, other.Name)
	if err != nil {
		return secretAdminOutcome{}, err
	}
	if !bytes.Equal(storedHash, hash) || !bytes.Equal(storedSalt, salt) || !stored.MustReset || stored.AuthEpoch != other.AuthEpoch+1 {
		return secretAdminOutcome{}, errCredentialsChanged
	}
	if err = secretTargetRevokedQ(q, other.Name); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = ctx.Err(); err != nil {
		return secretAdminOutcome{}, err
	}
	if err = tx.Commit(); err != nil {
		return secretAdminOutcome{}, secretAdminCommitError{}
	}
	return secretAdminOutcome{kind: "resetpass", actor: u.Name, target: other.Name, secret: password, epoch: u.AuthEpoch, targetEpoch: other.AuthEpoch + 1, committed: true}, nil
}

func revokeSecretTargetQ(q queryer, target string) error {
	for _, table := range []string{"ssh_keys", "sessions"} {
		where := "user=? AND revoked=0"
		if table == "sessions" {
			where += " AND closed=0"
		}
		var expected int64
		if err := q.QueryRow("SELECT count(*) FROM "+table+" WHERE "+where, target).Scan(&expected); err != nil {
			return err
		}
		result, err := q.Exec("UPDATE "+table+" SET revoked=1 WHERE "+where, target)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != expected {
			return errCredentialsChanged
		}
	}
	return secretTargetRevokedQ(q, target)
}
func secretTargetRevokedQ(q queryer, target string) error {
	var keys, sessions int
	if err := q.QueryRow(`SELECT (SELECT count(*) FROM ssh_keys WHERE user=? AND revoked=0),(SELECT count(*) FROM sessions WHERE user=? AND closed=0 AND revoked=0)`, target, target).Scan(&keys, &sessions); err != nil {
		return err
	}
	if keys != 0 || sessions != 0 {
		return errCredentialsChanged
	}
	return nil
}

// A committed operation is not rolled back by delivery authorization failure.
// Only the secret is suppressed; callers must keep a static committed notice.
func (s *Store) secretAdminDeliveryAllowed(ctx context.Context, outcome secretAdminOutcome, sessionID string) bool {
	if ctx == nil || sessionID == "" || !outcome.committed {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	tx, err := s.readDB().BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	u, err := secretAdminActorQ(q, outcome.actor, sessionID)
	if err != nil || u.AuthEpoch != outcome.epoch {
		return false
	}
	if outcome.kind == "resetpass" {
		other, e := userQ(q, outcome.target)
		if e != nil || other.Name == u.Name || roleRank(other.Role) >= roleRank(u.Role) || other.AuthEpoch != outcome.targetEpoch {
			return false
		}
	}
	return tx.Commit() == nil
}
