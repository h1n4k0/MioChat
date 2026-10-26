package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

const maxSSHKeysPerUser = 10

// SSHKey contains public material only. Private keys never leave the client.
type SSHKey struct {
	ID                                             int64
	User, Fingerprint, PublicKey, Algorithm, Label string
	Created, LastUsed                              int64
	Revoked                                        bool
}

// ValidateSSHKey constrains key formats and verification cost. RSA key encoding
// is named ssh-rsa; the transport separately permits only its SHA-2 signatures.
func ValidateSSHKey(key gossh.PublicKey) error {
	if key == nil {
		return errors.New("请提供有效的 SSH 公钥")
	}
	if _, ok := key.(*gossh.Certificate); ok {
		return errors.New("暂不接受 SSH 证书，请绑定原始公钥")
	}
	cryptoKey, ok := key.(gossh.CryptoPublicKey)
	if !ok {
		return errors.New("仅支持 Ed25519、ECDSA 或 RSA 公钥")
	}
	switch key.Type() {
	case gossh.KeyAlgoED25519:
		public, ok := cryptoKey.CryptoPublicKey().(ed25519.PublicKey)
		if !ok || len(public) != ed25519.PublicKeySize {
			return errors.New("无效的 Ed25519 公钥")
		}
	case gossh.KeyAlgoECDSA256, gossh.KeyAlgoECDSA384, gossh.KeyAlgoECDSA521:
		public, ok := cryptoKey.CryptoPublicKey().(*ecdsa.PublicKey)
		if !ok || public == nil || public.Curve == nil || public.X == nil || public.Y == nil {
			return errors.New("无效的 ECDSA 公钥")
		}
		curve := elliptic.P256()
		if key.Type() == gossh.KeyAlgoECDSA384 {
			curve = elliptic.P384()
		} else if key.Type() == gossh.KeyAlgoECDSA521 {
			curve = elliptic.P521()
		}
		if public.Curve != curve || !curve.IsOnCurve(public.X, public.Y) {
			return errors.New("ECDSA 仅支持 NIST P-256、P-384、P-521")
		}
	case gossh.KeyAlgoRSA:
		public, ok := cryptoKey.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || public == nil || public.N == nil || public.N.Sign() <= 0 || public.N.Bit(0) == 0 || public.E < 3 || public.E > 1<<31-1 || public.E%2 == 0 {
			return errors.New("无效的 RSA 公钥")
		}
		if public.N.BitLen() < 2048 || public.N.BitLen() > 8192 {
			return errors.New("RSA 公钥需要 2048–8192 位，建议使用 3072 位以上或 Ed25519")
		}
	default:
		return errors.New("仅支持 Ed25519、ECDSA 或 RSA 公钥，不接受 DSA 等旧算法")
	}
	return nil
}

// ParseSSHKey accepts exactly one standard .pub line, with an optional comment.
// authorized_keys options are intentionally rejected rather than ignored.
func ParseSSHKey(raw string) (gossh.PublicKey, string, error) {
	if len(raw) > 16*1024 {
		return nil, "", errors.New("公钥内容过长")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n") || strings.Contains(raw, "PRIVATE KEY") || strings.HasPrefix(raw, "-----") {
		return nil, "", errors.New("请粘贴一行 .pub 公钥，不要提供私钥或多行内容")
	}
	key, _, options, rest, err := gossh.ParseAuthorizedKey([]byte(raw))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, "", errors.New("公钥格式无效；请仅提供一行不带 authorized_keys 选项的公钥")
	}
	if err := ValidateSSHKey(key); err != nil {
		return nil, "", err
	}
	return key, strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key))), nil
}

func sshKeyAlgorithm(key gossh.PublicKey) string {
	switch key.Type() {
	case gossh.KeyAlgoED25519:
		return "Ed25519"
	case gossh.KeyAlgoECDSA256:
		return "ECDSA P-256"
	case gossh.KeyAlgoECDSA384:
		return "ECDSA P-384"
	case gossh.KeyAlgoECDSA521:
		return "ECDSA P-521"
	case gossh.KeyAlgoRSA:
		public := key.(gossh.CryptoPublicKey).CryptoPublicKey().(*rsa.PublicKey)
		return fmt.Sprintf("RSA %d / SHA-2", public.N.BitLen())
	}
	return ""
}

const sshKeyProjection = `id,user,fingerprint,public_key,algorithm,label,created,last_used,revoked`

type sshKeyScanner interface{ Scan(...any) error }

func scanSSHKey(row sshKeyScanner) (SSHKey, error) {
	var key SSHKey
	err := row.Scan(&key.ID, &key.User, &key.Fingerprint, &key.PublicKey, &key.Algorithm, &key.Label, &key.Created, &key.LastUsed, &key.Revoked)
	return key, err
}

func checkedSSHKeyWrite(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("密钥记录未成功更新，请重试")
	}
	return nil
}

func auditSSHKeyQ(q queryer, actor, action, detail string) error {
	// A trigger may ignore an INSERT without returning an SQL error. A key
	// operation is committed only when its audit row was actually written.
	return checkedSSHKeyWrite(q.Exec(`INSERT INTO audit(actor,action,target,detail,room,at) VALUES(?,?,?,?,'',?)`, actor, action, actor, cleanText(detail, 1200), time.Now().Unix()))
}

func sshKeyUserQ(q queryer, name string) (User, error) {
	u, err := userQ(q, name)
	if err != nil {
		return User{}, errors.New("账户不可用")
	}
	var banned int
	if err = q.QueryRow(`SELECT count(*) FROM sanctions WHERE kind='user' AND target=? AND revoked=0 AND (expires=0 OR expires>?)`, u.Name, time.Now().Unix()).Scan(&banned); err != nil {
		return User{}, err
	}
	if u.Banned || banned > 0 {
		return User{}, errors.New("此账户已被管理员停用")
	}
	if u.MustReset {
		return User{}, errors.New("请先使用临时密码登录并修改密码，再使用密钥")
	}
	// Maintenance keeps existing password login available; key login follows the
	// same policy, while canSendQ still enforces maintenance for posting.
	return u, nil
}

// BindSSHKey is a trusted-store operation, called only after the application has
// authenticated the account AND verified possession of this exact key through
// the current SSH session's completed signature. Never expose this method as an
// arbitrary public-key-paste command: public material alone proves no ownership.
func (s *Store) BindSSHKey(user, label, raw string) (SSHKey, error) {
	return s.bindSSHKey("", user, label, raw)
}

// BindSSHKeyForSession additionally gates a user-facing binding against the
// current durable session in the same transaction as the credential write.
func (s *Store) BindSSHKeyForSession(sessionID, user, label, raw string) (SSHKey, error) {
	if strings.TrimSpace(sessionID) == "" {
		return SSHKey{}, errors.New("当前会话不可用，请重新登录")
	}
	return s.bindSSHKey(sessionID, user, label, raw)
}

func (s *Store) bindSSHKey(sessionID, user, label, raw string) (SSHKey, error) {
	public, normalized, err := ParseSSHKey(raw)
	if err != nil {
		return SSHKey{}, err
	}
	user = accountKey(user)
	label = cleanText(label, 48)
	if label == "" {
		label = "我的 " + sshKeyAlgorithm(public) + " 密钥"
	}
	fingerprint := gossh.FingerprintSHA256(public)
	tx, err := s.db.Begin()
	if err != nil {
		return SSHKey{}, err
	}
	defer tx.Rollback()
	if _, err = sshKeyUserQ(tx, user); err != nil {
		return SSHKey{}, err
	}
	if sessionID != "" {
		var valid bool
		valid, err = sessionCurrentQ(tx, sessionID, user)
		if err != nil {
			return SSHKey{}, err
		}
		if !valid {
			return SSHKey{}, errors.New("当前会话已失效，请重新登录后绑定密钥")
		}
	}
	key, found := scanSSHKey(tx.QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE fingerprint=?`, fingerprint))
	if found != nil && !errors.Is(found, sql.ErrNoRows) {
		return SSHKey{}, found
	}
	if found == nil {
		if key.User != user || key.PublicKey != normalized {
			return SSHKey{}, errors.New("该公钥不可绑定，请使用其他密钥")
		}
		if !key.Revoked {
			return key, nil
		}
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM ssh_keys WHERE user=? AND revoked=0`, user).Scan(&count); err != nil {
		return SSHKey{}, err
	}
	if count >= maxSSHKeysPerUser {
		return SSHKey{}, fmt.Errorf("每个账户最多绑定 %d 把有效密钥，请先移除不用的密钥", maxSSHKeysPerUser)
	}
	now := time.Now().Unix()
	if found == nil {
		// Rebinding cannot resurrect any session authenticated before revocation.
		if _, err = tx.Exec(`UPDATE sessions SET revoked=1 WHERE id IN (SELECT session_id FROM session_credentials WHERE key_id=?)`, key.ID); err != nil {
			return SSHKey{}, err
		}
		err = checkedSSHKeyWrite(tx.Exec(`UPDATE ssh_keys SET label=?,algorithm=?,created=?,last_used=0,revoked=0 WHERE id=? AND user=?`, label, sshKeyAlgorithm(public), now, key.ID, user))
	} else {
		var result sql.Result
		result, err = tx.Exec(`INSERT INTO ssh_keys(user,fingerprint,public_key,algorithm,label,created) VALUES(?,?,?,?,?,?)`, user, fingerprint, normalized, sshKeyAlgorithm(public), label, now)
		err = checkedSSHKeyWrite(result, err)
		if err == nil {
			key.ID, err = result.LastInsertId()
		}
	}
	if err != nil {
		return SSHKey{}, err
	}
	if err = auditSSHKeyQ(tx, user, "sshkey.bind", fmt.Sprintf("#%d %s %s", key.ID, fingerprint, label)); err != nil {
		return SSHKey{}, err
	}
	key, err = scanSSHKey(tx.QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE id=? AND user=?`, key.ID, user))
	if err != nil {
		return SSHKey{}, err
	}
	if err = tx.Commit(); err != nil {
		return SSHKey{}, err
	}
	return key, nil
}

// SSHKeys lists only the owner's active keys, never another account's keys.
func (s *Store) SSHKeys(user string) ([]SSHKey, error) {
	rows, err := s.readDB().Query(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE user=? AND revoked=0 ORDER BY id`, accountKey(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]SSHKey, 0)
	for rows.Next() {
		key, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) RevokeSSHKey(user string, id int64) error {
	user = accountKey(user)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key, err := scanSSHKey(tx.QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE id=? AND user=?`, id, user))
	if err != nil {
		return errors.New("密钥不存在或不属于当前账户")
	}
	if err = checkedSSHKeyWrite(tx.Exec(`UPDATE ssh_keys SET revoked=1 WHERE id=? AND user=?`, id, user)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE sessions SET revoked=1 WHERE id IN (SELECT session_id FROM session_credentials WHERE key_id=?)`, id); err != nil {
		return err
	}
	if !key.Revoked {
		if err = auditSSHKeyQ(tx, user, "sshkey.revoke", fmt.Sprintf("#%d %s", id, key.Fingerprint)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RenameSSHKey(user string, id int64, label string) error {
	user, label = accountKey(user), cleanText(label, 48)
	if label == "" {
		return errors.New("密钥备注不能为空")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	key, err := scanSSHKey(tx.QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE id=? AND user=? AND revoked=0`, id, user))
	if err != nil {
		return errors.New("密钥不存在或不属于当前账户")
	}
	if err = checkedSSHKeyWrite(tx.Exec(`UPDATE ssh_keys SET label=? WHERE id=? AND user=? AND revoked=0`, label, id, user)); err != nil {
		return err
	}
	if err = auditSSHKeyQ(tx, user, "sshkey.rename", fmt.Sprintf("#%d %s %s", id, key.Fingerprint, label)); err != nil {
		return err
	}
	return tx.Commit()
}

// FindSSHKey is a read-only lookup. It does not authenticate a client or mark a
// key as used; callers must not trust an unsigned SSH public-key probe.
func (s *Store) FindSSHKey(public gossh.PublicKey) (SSHKey, error) {
	if err := ValidateSSHKey(public); err != nil {
		return SSHKey{}, err
	}
	key, err := scanSSHKey(s.readDB().QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE fingerprint=? AND public_key=? AND revoked=0`, gossh.FingerprintSHA256(public), strings.TrimSpace(string(gossh.MarshalAuthorizedKey(public)))))
	if err != nil {
		return SSHKey{}, errors.New("密钥未绑定或已停用")
	}
	return key, nil
}

// AuthenticateSSHKey must be called only after SSH has verified the signature.
// Lookup, current account restrictions and last-used update share a transaction.
func (s *Store) AuthenticateSSHKey(public gossh.PublicKey) (User, error) {
	if err := ValidateSSHKey(public); err != nil {
		return User{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	key, err := scanSSHKey(tx.QueryRow(`SELECT `+sshKeyProjection+` FROM ssh_keys WHERE fingerprint=? AND public_key=? AND revoked=0`, gossh.FingerprintSHA256(public), strings.TrimSpace(string(gossh.MarshalAuthorizedKey(public)))))
	if err != nil {
		return User{}, errors.New("密钥未绑定或已停用")
	}
	u, err := sshKeyUserQ(tx, key.User)
	if err != nil {
		return User{}, err
	}
	if err = checkedSSHKeyWrite(tx.Exec(`UPDATE ssh_keys SET last_used=? WHERE id=? AND revoked=0`, time.Now().Unix(), key.ID)); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return u, nil
}
