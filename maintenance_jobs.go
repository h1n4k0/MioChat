package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const snapshotPendingName = ".snapshot-in-progress"
const snapshotReserveBytes = uint64(64 * 1024 * 1024)

type maintenanceReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r maintenanceReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(buffer)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return n, cancelled
	}
	return n, err
}

func maintenanceError(ctx context.Context, err error) error {
	if cancelled := ctx.Err(); cancelled != nil {
		return cancelled
	}
	return err
}

type maintenanceQueryer struct {
	ctx context.Context
	db  *sql.DB
}

func (q maintenanceQueryer) QueryRow(query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(q.ctx, query, args...)
}
func (q maintenanceQueryer) Query(query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(q.ctx, query, args...)
}
func (q maintenanceQueryer) Exec(query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(q.ctx, query, args...)
}

func requiredSnapshotSpace(source string) (uint64, error) {
	var total uint64
	for i, name := range []string{source, source + "-wal"} {
		info, err := os.Stat(name)
		if i == 1 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return 0, errors.New("数据库或 WAL 文件类型无效")
		}
		size := uint64(info.Size())
		if size > (^uint64(0)-snapshotReserveBytes)/2-total {
			return 0, errors.New("备份容量估计过大")
		}
		total += size
	}
	return total*2 + snapshotReserveBytes, nil
}

func snapshotCapacity(source, destination string, freeSpace func(string) (uint64, error)) error {
	required, err := requiredSnapshotSpace(source)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	for {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return errors.New("备份目标父路径不是目录")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return errors.New("找不到可用的备份目标父目录")
		}
		parent = next
	}
	available, err := freeSpace(parent)
	if err != nil {
		return err
	}
	if available < required {
		return fmt.Errorf("备份空间不足：需要至少 %.1f MiB，可用 %.1f MiB；请扩容或选择其他目录", float64(required)/(1<<20), float64(available)/(1<<20))
	}
	return nil
}

// RunMaintenance admits one maintenance operation per running store. The caller
// supplies its session context, so disconnecting or cancelling stops SQL and IO.
func (s *Store) RunMaintenance(ctx context.Context, actor, kind string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind != "backup" && kind != "check" {
		return nil, errors.New("维护作业类型无效")
	}
	if !s.maintenanceMu.TryLock() {
		return nil, errors.New("已有维护作业正在运行，请稍后再试")
	}
	defer s.maintenanceMu.Unlock()
	u, err := currentCredentialUserQ(maintenanceQueryer{ctx: ctx, db: s.readDB()}, actor)
	if err != nil {
		return nil, maintenanceError(ctx, err)
	}
	if !isAdmin(u.Role) || u.MustReset {
		return nil, errors.New("需要已完成密码设置的管理员权限")
	}
	if kind == "check" {
		return s.ChecksContext(ctx)
	}
	if s.path == ":memory:" {
		return nil, errors.New("内存数据库不执行磁盘备份")
	}
	state := filepath.Dir(s.path)
	destination := filepath.Join(state, "backups", "backup-"+time.Now().UTC().Format("20060102T150405.000000000"))
	if _, err = makeSnapshotFileContext(ctx, s.path, state, destination); err != nil {
		return nil, err
	}
	lines := []string{destination, "包括数据库、主机密钥与摘要清单。"}
	// The snapshot is already committed. Record its outcome with a bounded
	// context even if the client disconnected immediately after completion.
	auditContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err = auditQ(maintenanceQueryer{ctx: auditContext, db: s.db.DB}, actor, "backup.create", filepath.Base(destination), "", ""); err != nil {
		return lines, fmt.Errorf("备份已保存，但审计写入失败：%w", err)
	}
	return lines, nil
}
