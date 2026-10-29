package main

import (
	"context"
	"errors"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// Set only from Session.PublicKey after the SSH signature has been verified.
// Neither an unsigned public-key probe nor a GUI JSON field can set this value.
type guiSSHProofKey struct{}

type guiDeviceKey struct {
	Algorithm   string `json:"algorithm"`
	Fingerprint string `json:"fingerprint"`
	Bound       bool   `json:"bound"`
}

func guiSignedKey(ctx context.Context) gossh.PublicKey {
	key, _ := ctx.Value(guiSSHProofKey{}).(gossh.PublicKey)
	if ValidateSSHKey(key) != nil {
		return nil
	}
	return key
}

func guiDeviceKeyInfo(store *Store, key gossh.PublicKey) *guiDeviceKey {
	if ValidateSSHKey(key) != nil {
		return nil
	}
	_, err := store.FindSSHKey(key)
	return &guiDeviceKey{Algorithm: sshKeyAlgorithm(key), Fingerprint: gossh.FingerprintSHA256(key), Bound: err == nil}
}

func guiBindDeviceKey(ctx context.Context, store *Store, hub *Hub, user User, client *Client, verifiedAt *time.Time, request guiRequest) (guiResponse, error) {
	response := guiResponse{Type: "key_bound", RequestID: request.RequestID}
	key := guiSignedKey(ctx)
	if key == nil {
		return response, errors.New("本次 SSH 连接没有已签名的设备密钥；请选择密钥后重新连接，不要粘贴公钥")
	}
	if !hub.authAllowed(client.IP, false) {
		return response, errors.New("验证过于频繁，请稍后重试")
	}
	select {
	case hub.AuthSlots <- struct{}{}:
	case <-ctx.Done():
		return response, ctx.Err()
	}
	defer func() { <-hub.AuthSlots }()
	fresh := !verifiedAt.IsZero() && time.Since(*verifiedAt) < 2*time.Minute
	if !fresh || request.Password != "" {
		u, err := store.Authenticate(user.Name, request.Password)
		if err != nil || u.MustReset || u.AuthEpoch != client.AuthEpoch {
			return response, errors.New("需要重新验证当前账户密码")
		}
		*verifiedAt = time.Now()
	}
	if ctx.Err() != nil {
		return response, ctx.Err()
	}
	_, err := store.BindSSHKeyForSessionContext(ctx, client.ID, user.Name, request.Body, string(gossh.MarshalAuthorizedKey(key)))
	if err != nil {
		return response, err
	}
	response.DeviceKey = guiDeviceKeyInfo(store, key)
	response.Status = "本次已验证的设备密钥已绑定；下次可用同一密钥登录"
	return response, nil
}
