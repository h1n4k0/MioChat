package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var errCredentialsChanged = errors.New("账号凭据或会话已更新，请重新登录")

func (s *Store) registrationUser(name string) (User, error) {
	u, err := s.User(name)
	// Registration proves only the generation created by that registration,
	// never a password reset that raced the post-commit profile read.
	u.AuthEpoch = 0
	return u, err
}

// A password proof records exactly the credential generation that was checked.
// Expensive hashing happens without a SQL transaction or pooled connection held.
type passwordProof struct {
	name       string
	hash, salt []byte
	epoch      int64
}

func currentCredentialUserQ(q queryer, name string) (User, error) {
	u, err := userQ(q, name)
	if err != nil {
		return User{}, err
	}
	var banned int
	if err = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='user' AND target=? AND revoked=0 AND (expires=0 OR expires>?)`, u.Name, time.Now().Unix()).Scan(&banned); err != nil {
		return User{}, err
	}
	if u.Banned || banned != 0 {
		return User{}, errors.New("此账户已被管理员停用")
	}
	return u, nil
}

func (s *Store) verifyPassword(name, password string) (User, passwordProof, error) {
	name = accountKey(name)
	if len(password) > 256 {
		return User{}, passwordProof{}, errors.New("用户名或密码不正确")
	}
	proof := passwordProof{name: name}
	err := s.readDB().QueryRow(`SELECT u.hash,u.salt,COALESCE(v.epoch,0) FROM users u LEFT JOIN credential_versions v ON v.user=u.name WHERE u.name=?`, name).Scan(&proof.hash, &proof.salt, &proof.epoch)
	if err != nil {
		passwordHash(password, s.dummySalt)
		return User{}, passwordProof{}, errors.New("用户名或密码不正确")
	}
	if subtle.ConstantTimeCompare(proof.hash, passwordHash(password, proof.salt)) != 1 {
		return User{}, passwordProof{}, errors.New("用户名或密码不正确")
	}
	u, err := currentCredentialUserQ(s.readDB(), name)
	if err != nil {
		return User{}, passwordProof{}, err
	}
	if u.AuthEpoch != proof.epoch {
		return User{}, passwordProof{}, errCredentialsChanged
	}
	// Do not replace the proof with any more recent generation. Session creation
	// must atomically compare this value again after this method returns.
	u.AuthEpoch = proof.epoch
	return u, proof, nil
}

func credentialWrite(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errCredentialsChanged
	}
	return nil
}

func setCredentialEpochQ(q queryer, user string, epoch int64) error {
	return credentialWrite(q.Exec(`INSERT INTO credential_versions(user,epoch) VALUES(?,?) ON CONFLICT(user) DO UPDATE SET epoch=excluded.epoch`, user, epoch))
}

// Missing generation metadata means generation zero, preserving pre-migration
// sessions. A subsequent password change makes these old sessions invalid.
func sessionCurrentQ(q queryer, id, user string) (bool, error) {
	var valid int
	err := q.QueryRow(`SELECT count(*) FROM sessions s JOIN users u ON u.name=s.user LEFT JOIN session_versions v ON v.session_id=s.id LEFT JOIN credential_versions c ON c.user=s.user WHERE s.id=? AND (?='' OR s.user=?) AND s.closed=0 AND s.revoked=0 AND u.banned=0 AND COALESCE(v.epoch,0)=COALESCE(c.epoch,0) AND NOT EXISTS(SELECT 1 FROM sanctions a WHERE a.kind='user' AND a.target=s.user AND a.revoked=0 AND (a.expires=0 OR a.expires>?)) AND NOT EXISTS(SELECT 1 FROM session_credentials k LEFT JOIN ssh_keys p ON p.id=k.key_id WHERE k.session_id=s.id AND (p.id IS NULL OR p.user!=s.user OR p.revoked!=0))`, id, user, user, time.Now().Unix()).Scan(&valid)
	return valid == 1, err
}

func (s *Store) ChangePasswordForSession(name, old, next, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return errCredentialsChanged
	}
	return s.changePassword(name, old, next, sessionID)
}

func (s *Store) changePassword(name, old, next, sessionID string) error {
	if utf8.RuneCountInString(next) < 10 || len(next) > 256 {
		return errors.New("新密码至少 10 个字符，且不超过 256 字节")
	}
	_, proof, err := s.verifyPassword(name, old)
	if err != nil {
		return err
	}
	salt := make([]byte, 16)
	if _, err = rand.Read(salt); err != nil {
		return err
	}
	hash := passwordHash(next, salt)
	return s.commitPasswordChange(proof, hash, salt, sessionID)
}

// The proof and replacement hash are prepared outside this short transaction.
// A reset or competing change invalidates the proof before any state is changed.
func (s *Store) commitPasswordChange(proof passwordProof, hash, salt []byte, sessionID string) error {
	if proof.epoch < 0 || proof.epoch == math.MaxInt64 {
		return errCredentialsChanged
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := currentCredentialUserQ(tx, proof.name)
	if err != nil {
		return err
	}
	if u.AuthEpoch != proof.epoch {
		return errCredentialsChanged
	}
	if sessionID != "" {
		valid, err := sessionCurrentQ(tx, sessionID, proof.name)
		if err != nil {
			return err
		}
		if !valid {
			return errCredentialsChanged
		}
	}
	if err = credentialWrite(tx.Exec(`UPDATE users SET hash=?,salt=? WHERE name=? AND hash=? AND salt=? AND COALESCE((SELECT epoch FROM credential_versions WHERE user=users.name),0)=?`, hash, salt, proof.name, proof.hash, proof.salt, proof.epoch)); err != nil {
		return err
	}
	nextEpoch := proof.epoch + 1
	if err = setCredentialEpochQ(tx, proof.name, nextEpoch); err != nil {
		return err
	}
	if err = credentialWrite(tx.Exec(`UPDATE user_meta SET must_reset=0 WHERE name=?`, proof.name)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE sessions SET revoked=1 WHERE user=? AND closed=0 AND id!=?`, proof.name, sessionID); err != nil {
		return err
	}
	if sessionID != "" {
		if err = credentialWrite(tx.Exec(`INSERT INTO session_versions(session_id,epoch) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET epoch=excluded.epoch`, sessionID, nextEpoch)); err != nil {
			return err
		}
	}
	if err = auditQ(tx, proof.name, "password.change", proof.name, "", ""); err != nil {
		return err
	}
	return tx.Commit()
}
