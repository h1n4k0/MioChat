package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Only aggregates cross the native protocol: never account names, addresses,
// message bodies or database paths. A failed query is not a zero-valued result.
type guiStatistics struct {
	Version        string  `json:"version"`
	Users          int64   `json:"users"`
	Messages       int64   `json:"messages"`
	ActiveSessions int64   `json:"active_sessions"`
	MinuteMessages int64   `json:"minute_messages"`
	HourLimited    int64   `json:"hour_limited"`
	HeapBytes      uint64  `json:"heap_bytes"`
	Goroutines     int     `json:"goroutines"`
	UptimeSeconds  int64   `json:"uptime_seconds"`
	OnlineSessions int     `json:"online_sessions"`
	DatabaseBytes  *int64  `json:"database_bytes,omitempty"`
	FreeDiskBytes  *uint64 `json:"free_disk_bytes,omitempty"`
	StorageNote    string  `json:"storage_note,omitempty"`
}

func guiStatisticsSnapshot(parent context.Context, store *Store, hub *Hub, now time.Time) (*guiStatistics, error) {
	if parent == nil {
		return nil, errors.New("missing request context")
	}
	ctx, cancel := context.WithTimeout(parent, 300*time.Millisecond)
	defer cancel()
	result, err := guiStatisticsDataQ(scopedQuery{ctx: ctx, db: store.readDB()}, now)
	if err != nil {
		return nil, err
	}
	guiStatisticsRuntime(store, hub, result, now)
	return result, nil
}

// Caller owns the snapshot and DB deadline, including acquisition and scan.
func guiStatisticsDataQ(q queryer, now time.Time) (*guiStatistics, error) {
	result := &guiStatistics{Version: appVersion}
	// A single reader statement gives a consistent aggregate snapshot and keeps
	// connection acquisition, scanning and cancellation within one read budget.
	err := q.QueryRow(`SELECT
 (SELECT count(*) FROM users), (SELECT count(*) FROM messages),
 (SELECT count(*) FROM sessions WHERE closed=0 AND revoked=0),
 (SELECT count(*) FROM messages WHERE at>=?),
 (SELECT COALESCE(sum(count),0) FROM rate_stats WHERE hour=?)`, now.Unix()-60, now.Unix()/3600).Scan(
		&result.Users, &result.Messages, &result.ActiveSessions, &result.MinuteMessages, &result.HourLimited)
	if err != nil {
		return nil, fmt.Errorf("读取运行统计失败: %w", err)
	}
	return result, nil
}

// Runtime/Hub/filesystem work is outside the database snapshot and its budget.
func guiStatisticsRuntime(store *Store, hub *Hub, result *guiStatistics, now time.Time) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	result.HeapBytes, result.Goroutines = mem.HeapAlloc, runtime.NumGoroutine()
	hub.mu.Lock()
	result.UptimeSeconds = max(0, int64(now.Sub(hub.Started)/time.Second))
	result.OnlineSessions = len(hub.clients)
	hub.mu.Unlock()
	if store.path != ":memory:" {
		if info, e := os.Stat(store.path); e == nil {
			size := info.Size()
			result.DatabaseBytes = &size
		} else {
			result.StorageNote = "数据库主文件大小暂不可用"
		}
		if free, e := diskFree(filepath.Dir(store.path)); e == nil {
			value := uint64(free)
			result.FreeDiskBytes = &value
			if value < 1<<30 {
				result.StorageNote += " 可用空间不足 1 GiB；请扩容或备份迁移，不会自动删除历史"
			}
		} else {
			result.StorageNote += " 可用磁盘空间暂不可用"
		}
	}
}

func guiInformationPanel(store *Store, hub *Hub, request guiRequest, command string) (*guiPanel, error) {
	panel := &guiPanel{Kind: command[1:]}
	switch command {
	case "/stats":
		panel.Title = "运行看板"
		stats, err := guiStatisticsSnapshot(request.ctx, store, hub, time.Now())
		if err != nil {
			return nil, err
		}
		panel.Statistics = stats
	case "/time":
		panel.Title = "服务器时间 · 北京时间"
		panel.ServerTime = time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05 CST")
	case "/art":
		panel.Title = "MioChat 营地"
		panel.Lines = []string{campArt}
	}
	return panel, nil
}
