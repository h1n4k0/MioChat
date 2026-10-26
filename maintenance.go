package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

type SnapshotManifest struct {
	Version                                        string
	Schema                                         int
	Created                                        string
	DatabaseSHA256, HostKeySHA256, HostFingerprint string
}

func digestFile(path string) (string, error) {
	return digestFileContext(context.Background(), path)
}
func digestFileContext(ctx context.Context, path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	hash := sha256.New()
	if _, e = io.Copy(hash, maintenanceReader{ctx: ctx, reader: f}); e != nil {
		return "", e
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func copyPrivate(source, target string) error {
	return copyPrivateContext(context.Background(), source, target)
}
func copyPrivateContext(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, e := os.Open(source)
	if e != nil {
		return e
	}
	defer src.Close()
	out, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = io.Copy(out, maintenanceReader{ctx: ctx, reader: src}); e != nil {
		out.Close()
		return e
	}
	if e = out.Sync(); e != nil {
		out.Close()
		return e
	}
	return out.Close()
}
func MakeSnapshot(state, destination string) (SnapshotManifest, error) {
	return MakeSnapshotContext(context.Background(), state, destination)
}
func MakeSnapshotContext(ctx context.Context, state, destination string) (SnapshotManifest, error) {
	if err := ensureNoPendingRestore(state); err != nil {
		return SnapshotManifest{}, err
	}
	return makeSnapshotFileContext(ctx, filepath.Join(state, "miochat.db"), state, destination)
}
func makeSnapshotFile(source, state, destination string) (SnapshotManifest, error) {
	return makeSnapshotFileContext(context.Background(), source, state, destination)
}
func makeSnapshotFileContext(ctx context.Context, source, state, destination string) (SnapshotManifest, error) {
	var manifest SnapshotManifest
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	if err := ensureNoPendingRestore(state); err != nil {
		return manifest, err
	}
	if info, e := os.Stat(source); e != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return manifest, errors.New("源数据库不存在或为空")
	}
	if _, e := os.Stat(destination); !errors.Is(e, os.ErrNotExist) {
		return manifest, errors.New("备份目标必须是全新目录，不能覆盖已有备份")
	}
	if err := snapshotCapacity(source, destination, diskFree); err != nil {
		return manifest, err
	}
	if e := os.MkdirAll(destination, 0700); e != nil {
		return manifest, e
	}
	pending := filepath.Join(destination, snapshotPendingName)
	if err := writeSyncedPrivate(pending, []byte("incomplete snapshot\n")); err != nil {
		return manifest, err
	}
	completed := false
	defer func() {
		if !completed {
			// A failed or cancelled job must never be accepted as a restore source.
			_ = os.Rename(filepath.Join(destination, "manifest.json"), filepath.Join(destination, "manifest.incomplete"))
		}
	}()
	// Opening the DB doesn't migrate or write user data; VACUUM INTO creates a
	// transactionally consistent snapshot including committed WAL contents.
	db, e := sql.Open("sqlite", source)
	if e != nil {
		return manifest, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); e != nil {
		return manifest, maintenanceError(ctx, e)
	}
	var check string
	var tables int
	if e = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('users','messages')`).Scan(&tables); e != nil {
		return manifest, maintenanceError(ctx, e)
	}
	if tables != 2 {
		return manifest, errors.New("源数据库不是有效的 MioChat 数据库")
	}
	if e = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); e != nil {
		return manifest, maintenanceError(ctx, e)
	}
	if check != "ok" {
		return manifest, errors.New("源数据库校验失败")
	}
	if e = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&manifest.Schema); e != nil {
		return manifest, maintenanceError(ctx, e)
	}
	target := filepath.Join(destination, "miochat.db")
	if _, e = db.ExecContext(ctx, "VACUUM INTO ?", target); e != nil {
		return manifest, maintenanceError(ctx, e)
	}
	if e = os.Chmod(target, 0600); e != nil {
		return manifest, e
	}
	if e = syncPrivateFile(target); e != nil {
		return manifest, e
	}
	manifest.DatabaseSHA256, e = digestFileContext(ctx, target)
	if e != nil {
		return manifest, e
	}
	keyPath := filepath.Join(state, "host_ed25519")
	if info, keyErr := os.Lstat(keyPath); keyErr == nil {
		if !info.Mode().IsRegular() || info.Size() > 64*1024 {
			return manifest, errors.New("主机密钥文件类型无效或过大")
		}
		if e = copyPrivateContext(ctx, keyPath, filepath.Join(destination, "host_ed25519")); e != nil {
			return manifest, e
		}
		copiedKey := filepath.Join(destination, "host_ed25519")
		manifest.HostKeySHA256, e = digestFileContext(ctx, copiedKey)
		if e != nil {
			return manifest, e
		}
		data, e := os.ReadFile(copiedKey)
		if e != nil {
			return manifest, e
		}
		signer, e := gossh.ParsePrivateKey(data)
		if e != nil {
			return manifest, e
		}
		manifest.HostFingerprint = gossh.FingerprintSHA256(signer.PublicKey())
	} else if !errors.Is(keyErr, os.ErrNotExist) {
		return manifest, keyErr
	}
	if err := ctx.Err(); err != nil {
		return manifest, err
	}
	manifest.Version = appVersion
	manifest.Created = time.Now().UTC().Format(time.RFC3339)
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if e = writeSyncedPrivate(filepath.Join(destination, "manifest.json"), data); e != nil {
		return manifest, e
	}
	if e = syncDirectory(destination); e != nil {
		return manifest, e
	}
	if e = syncDirectory(filepath.Dir(destination)); e != nil {
		return manifest, e
	}
	if e = os.Remove(pending); e != nil {
		return manifest, e
	}
	if e = syncDirectory(destination); e != nil {
		// Best effort recreation also makes an interrupted commit visibly invalid.
		_ = writeSyncedPrivate(pending, []byte("incomplete snapshot\n"))
		return manifest, e
	}
	completed = true
	return manifest, nil
}
func ValidateSnapshot(source string) (SnapshotManifest, error) {
	var manifest SnapshotManifest
	if _, err := os.Lstat(filepath.Join(source, snapshotPendingName)); err == nil {
		return manifest, errors.New("备份作业未完成，不能用于恢复")
	} else if !errors.Is(err, os.ErrNotExist) {
		return manifest, err
	}
	for _, name := range []string{"manifest.json", "miochat.db"} {
		info, err := os.Lstat(filepath.Join(source, name))
		if err != nil || !info.Mode().IsRegular() || (name == "manifest.json" && info.Size() > 64*1024) {
			return manifest, errors.New("备份文件不存在、类型无效或清单过大")
		}
	}
	data, e := os.ReadFile(filepath.Join(source, "manifest.json"))
	if e != nil {
		return manifest, e
	}
	if e = json.Unmarshal(data, &manifest); e != nil {
		return manifest, e
	}
	sum, e := digestFile(filepath.Join(source, "miochat.db"))
	if e != nil || sum != manifest.DatabaseSHA256 {
		return manifest, errors.New("备份数据库摘要不匹配")
	}
	if manifest.HostKeySHA256 != "" {
		info, err := os.Lstat(filepath.Join(source, "host_ed25519"))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
			return manifest, errors.New("备份主机密钥文件无效")
		}
		sum, e = digestFile(filepath.Join(source, "host_ed25519"))
		if e != nil || sum != manifest.HostKeySHA256 {
			return manifest, errors.New("备份主机密钥摘要不匹配")
		}
		key, e := os.ReadFile(filepath.Join(source, "host_ed25519"))
		if e != nil {
			return manifest, e
		}
		signer, e := gossh.ParsePrivateKey(key)
		if e != nil || gossh.FingerprintSHA256(signer.PublicKey()) != manifest.HostFingerprint {
			return manifest, errors.New("备份主机密钥无效或指纹不匹配")
		}
	} else if manifest.HostFingerprint != "" {
		return manifest, errors.New("备份主机身份信息不完整")
	}
	db, e := sql.Open("sqlite", filepath.Join(source, "miochat.db"))
	if e != nil {
		return manifest, e
	}
	defer db.Close()
	var check string
	if e = db.QueryRow("PRAGMA quick_check").Scan(&check); e != nil || check != "ok" {
		return manifest, errors.New("备份数据库校验失败")
	}
	var version int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return manifest, e
	}
	if version < 0 || version != manifest.Schema || version > schemaVersion {
		return manifest, errors.New("备份数据库版本不受支持")
	}
	var tables int
	if e = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('users','messages')`).Scan(&tables); e != nil || tables != 2 {
		return manifest, errors.New("备份不是有效的 MioChat 数据库")
	}
	return manifest, nil
}
func RestoreSnapshot(state, source string) error {
	return restoreSnapshotWithOps(state, source, restoreOperations{replace: replaceFile, syncDirectory: syncDirectory})
}
func (s *Store) Backup(actor string) (string, error) {
	lines, err := s.RunMaintenance(context.Background(), actor, "backup")
	if len(lines) > 0 {
		return lines[0], err
	}
	return "", err
}
func (s *Store) Checks() ([]string, error) {
	return s.ChecksContext(context.Background())
}
func (s *Store) ChecksContext(ctx context.Context) ([]string, error) {
	var check string
	if e := s.readDB().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); e != nil {
		return nil, maintenanceError(ctx, e)
	}
	if check != "ok" {
		return nil, errors.New(check)
	}
	var version int
	if e := s.readDB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil {
		return nil, maintenanceError(ctx, e)
	}
	if version != schemaVersion {
		return nil, errors.New("数据库迁移版本不匹配")
	}
	var retention string
	if e := s.readDB().QueryRowContext(ctx, `SELECT value FROM config WHERE key='retention'`).Scan(&retention); e != nil {
		return nil, maintenanceError(ctx, e)
	}
	if retention != "permanent" {
		return nil, errors.New("检测到不允许的记录清理配置")
	}
	return []string{"SQLite quick_check: ok", fmt.Sprintf("schema: %d", version), "retention: permanent", "账号/消息/修订/审计均未启用自动清理"}, nil
}
func (s *Store) Metrics() []string {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	var users, messages, active, rate, limited int64
	_ = s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	_ = s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages)
	_ = s.db.QueryRow(`SELECT count(*) FROM sessions WHERE closed=0 AND revoked=0`).Scan(&active)
	_ = s.db.QueryRow(`SELECT count(*) FROM messages WHERE at>=?`, time.Now().Unix()-60).Scan(&rate)
	_ = s.db.QueryRow(`SELECT COALESCE(sum(count),0) FROM rate_stats WHERE hour=?`, time.Now().Unix()/3600).Scan(&limited)
	lines := []string{"MioChat " + appVersion, fmt.Sprintf("账户 %d · 消息 %d · 活跃会话 %d", users, messages, active), fmt.Sprintf("最近一分钟 %d 条 · 本小时限流 %d 次", rate, limited), fmt.Sprintf("Go 堆 %.1f MiB · Goroutine %d", float64(mem.HeapAlloc)/(1<<20), runtime.NumGoroutine()), ""}
	if s.path != ":memory:" {
		if info, e := os.Stat(s.path); e == nil {
			lines = append(lines, fmt.Sprintf("数据库主文件 %.1f MiB（不含 WAL）", float64(info.Size())/(1<<20)))
		}
		if free, e := diskFree(filepath.Dir(s.path)); e == nil {
			lines = append(lines, fmt.Sprintf("可用磁盘 %.2f GiB", float64(free)/(1<<30)))
			if free < 1<<30 {
				lines = append(lines, "磁盘空间不足 1 GiB，请扩容或备份迁移；程序不会删除历史")
			}
		}
	}
	return lines
}
func (s *Store) Dropped(count int64) {
	_, _ = s.db.Exec(`INSERT INTO rate_stats VALUES(?,?) ON CONFLICT(hour) DO UPDATE SET count=rate_stats.count+excluded.count`, time.Now().Unix()/3600, count)
}
