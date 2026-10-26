package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceRejectsConcurrentAndUnauthorizedJobs(t *testing.T) {
	s, _ := featureFixture(t)
	s.maintenanceMu.Lock()
	if _, err := s.RunMaintenance(context.Background(), "admin", "check"); err == nil {
		t.Fatal("a second maintenance operation was admitted")
	}
	s.maintenanceMu.Unlock()
	if _, err := s.RunMaintenance(context.Background(), "alice", "check"); err == nil {
		t.Fatal("ordinary account ran maintenance")
	}
	if _, err := s.db.Exec(`UPDATE users SET role='member' WHERE name='admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunMaintenance(context.Background(), "admin", "check"); err == nil {
		t.Fatal("demoted administrator ran maintenance using stale UI authority")
	}
	if _, err := s.db.Exec(`UPDATE users SET role='admin' WHERE name='admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword("owner", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunMaintenance(context.Background(), "admin", "check"); err == nil {
		t.Fatal("administrator with a reset password ran maintenance")
	}
	if _, err := s.RunMaintenance(context.Background(), "owner", "check"); err != nil {
		t.Fatal("completed or rejected job did not release its slot", err)
	}
	if _, err := s.RunMaintenance(context.Background(), "owner", "invalid-kind"); err == nil {
		t.Fatal("unknown maintenance operation was accepted")
	}
}

func TestMaintenanceCancellationCoversQueuedAuthorizationAndChecks(t *testing.T) {
	s, _ := featureFixture(t)
	first, err := s.readDB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := s.readDB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err = s.RunMaintenance(ctx, "admin", "check"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queued administrator check did not honor cancellation", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err = s.ChecksContext(ctx2); !errors.Is(err, context.Canceled) {
		t.Fatal("database check did not honor cancellation", err)
	}
	if !s.maintenanceMu.TryLock() {
		t.Fatal("cancelled job retained maintenance slot")
	}
	s.maintenanceMu.Unlock()
}

func TestSnapshotBudgetIncludesWALAndRejectsBeforeCreatingOutput(t *testing.T) {
	state := t.TempDir()
	source := filepath.Join(state, "source.db")
	if err := os.WriteFile(source, make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"-wal", make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	required, err := requiredSnapshotSpace(source)
	if err != nil || required != snapshotReserveBytes+2*(1024+2048) {
		t.Fatal("incorrect database plus WAL budget", required, err)
	}
	destination := filepath.Join(state, "not-created", "snapshot")
	checkedParent := ""
	if err = snapshotCapacity(source, destination, func(parent string) (uint64, error) {
		checkedParent = parent
		return required - 1, nil
	}); err == nil {
		t.Fatal("insufficient disk budget was accepted")
	}
	if checkedParent != state {
		t.Fatal("capacity was checked on the wrong filesystem", checkedParent)
	}
	if _, err = os.Stat(filepath.Dir(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capacity rejection created output directories")
	}
	if err = snapshotCapacity(source, destination, func(string) (uint64, error) { return required, nil }); err != nil {
		t.Fatal("sufficient capacity was rejected", err)
	}
}

func TestSnapshotCancelledBeforeStartDoesNotCreateFiles(t *testing.T) {
	state := restoreFixture(t, "snapshot-user")
	destination := filepath.Join(t.TempDir(), "cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MakeSnapshotContext(ctx, state, destination); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled snapshot started", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled snapshot created files")
	}
	if _, err := digestFileContext(ctx, filepath.Join(state, "miochat.db")); !errors.Is(err, context.Canceled) {
		t.Fatal("hashing ignored cancellation", err)
	}
	copyTarget := filepath.Join(t.TempDir(), "cancelled-copy")
	if err := copyPrivateContext(ctx, filepath.Join(state, "miochat.db"), copyTarget); !errors.Is(err, context.Canceled) {
		t.Fatal("file copying ignored cancellation", err)
	}
	if _, err := os.Stat(copyTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pre-cancelled copy created a destination")
	}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelAfterRead) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.cancel()
	return n, err
}

func TestMaintenanceReaderStopsWhenCancelledDuringIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := maintenanceReader{ctx: ctx, reader: cancelAfterRead{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 4096)), cancel: cancel}}
	if _, err := io.ReadAll(reader); !errors.Is(err, context.Canceled) {
		t.Fatal("stream copy or hashing could continue after cancellation", err)
	}
}

func TestCancelledSnapshotCannotBeRestoredAndSuccessfulJobIsValid(t *testing.T) {
	state := restoreFixture(t, "snapshot-user")
	db, err := sql.Open("sqlite", filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err = connection.ExecContext(context.Background(), "PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "cancelled")
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	_, err = MakeSnapshotContext(ctx, state, destination)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("SQL snapshot cancellation did not propagate", err)
	}
	if _, err = ValidateSnapshot(destination); err == nil {
		t.Fatal("cancelled partial snapshot was considered restorable")
	}
	if _, err = os.Stat(filepath.Join(destination, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled snapshot left a valid-looking manifest")
	}
	if _, err = connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if err = s.Promote("snapshot-user"); err != nil {
		t.Fatal(err)
	}
	lines, err := s.RunMaintenance(context.Background(), "snapshot-user", "backup")
	if err != nil || len(lines) == 0 {
		t.Fatal("background-capable backup failed", err)
	}
	if _, err = ValidateSnapshot(lines[0]); err != nil {
		t.Fatal("completed backup was not restorable", err)
	}
	if _, err = os.Stat(filepath.Join(lines[0], snapshotPendingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed snapshot retained incomplete marker")
	}
	// An interruption marker must reject even a manifest and database that would
	// otherwise validate. This also covers an interrupted final directory sync.
	if err = os.WriteFile(filepath.Join(lines[0], snapshotPendingName), []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateSnapshot(lines[0]); err == nil || !strings.Contains(err.Error(), "未完成") {
		t.Fatal("incomplete marker did not prevent restore", err)
	}
}
