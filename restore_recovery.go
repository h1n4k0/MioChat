package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const restoreMarkerName = ".miochat-restore.json"

type restoreJournal struct {
	Stage   string
	Schema  int
	DBHash  string
	KeyHash string
	Phase   string
}

type restoreOperations struct {
	replace       func(string, string) error
	syncDirectory func(string) error
}

func ensureNoPendingRestore(state string) error {
	if _, err := os.Lstat(filepath.Join(state, restoreMarkerName)); err == nil {
		return errors.New("状态目录存在未完成恢复，请先用当前程序独占打开以完成恢复")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncPrivateFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func writeSyncedPrivate(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func restoreSnapshotWithOps(state, source string, operations restoreOperations) error {
	manifest, err := ValidateSnapshot(source)
	if err != nil {
		return err
	}
	release, err := lockState(state)
	if err != nil {
		return err
	}
	defer release()
	if _, err = os.Stat(filepath.Join(state, "miochat.db")); err == nil {
		rescue := filepath.Join(state, "restore-rescue-"+time.Now().UTC().Format("20060102T150405.000000000"))
		if _, err = MakeSnapshot(state, rescue); err != nil {
			return fmt.Errorf("恢复前保存完整现状失败：%w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	token, err := secretToken()
	if err != nil {
		return err
	}
	stageName := ".restore-stage-" + token
	stage := filepath.Join(state, stageName)
	markerTemp := filepath.Join(state, ".restore-marker-"+token)
	if err = os.Mkdir(stage, 0700); err != nil {
		return err
	}
	defer func() {
		// Before the durable commit marker, a failed staging attempt has not
		// modified the target generation and can discard only its own copies.
		if ensureNoPendingRestore(state) == nil {
			cleanupRestoreStage(stage)
			_ = os.Remove(markerTemp)
		}
	}()
	for _, name := range []string{"miochat.db", "manifest.json", "host_ed25519"} {
		if name == "host_ed25519" && manifest.HostKeySHA256 == "" {
			continue
		}
		if err = copyPrivate(filepath.Join(source, name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	// Validate the copied generation again; the source may have changed while it
	// was being copied. No target file has been replaced at this point.
	manifest, err = ValidateSnapshot(stage)
	if err != nil {
		return err
	}
	if err = operations.syncDirectory(stage); err != nil {
		return err
	}
	journal := restoreJournal{Stage: stageName, Schema: manifest.Schema, DBHash: manifest.DatabaseSHA256, KeyHash: manifest.HostKeySHA256, Phase: "installing"}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err = writeSyncedPrivate(markerTemp, data); err != nil {
		return err
	}
	if err = operations.replace(markerTemp, filepath.Join(state, restoreMarkerName)); err != nil {
		return err
	}
	if err = operations.syncDirectory(state); err != nil {
		return err
	}
	return finishPendingRestore(state, journal, operations)
}

// Called only while holding the state lock, before opening the database.
func recoverPendingRestore(state string) error {
	marker := filepath.Join(state, restoreMarkerName)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return errors.New("无效恢复标记")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	var journal restoreJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		return err
	}
	return finishPendingRestore(state, journal, restoreOperations{replace: replaceFile, syncDirectory: syncDirectory})
}

func finishPendingRestore(state string, journal restoreJournal, operations restoreOperations) error {
	if !strings.HasPrefix(journal.Stage, ".restore-stage-") || filepath.Base(journal.Stage) != journal.Stage || len(journal.Stage) > 100 {
		return errors.New("恢复暂存路径无效")
	}
	if journal.Phase != "" && journal.Phase != "installing" && journal.Phase != "committed" {
		return errors.New("恢复提交阶段无效")
	}
	if journal.Phase == "committed" {
		return finishCommittedRestore(state, journal, operations)
	}
	stage := filepath.Join(state, journal.Stage)
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("恢复暂存目录无效")
	}
	manifest, err := ValidateSnapshot(stage)
	if err != nil || manifest.Schema != journal.Schema || manifest.DatabaseSHA256 != journal.DBHash || manifest.HostKeySHA256 != journal.KeyHash {
		return errors.New("恢复暂存内容与提交标记不一致")
	}
	// The requested generation excludes every previous WAL, even when its main
	// database happens to have the same digest as the requested backup.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(filepath.Join(state, "miochat.db") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range []string{"miochat.db", "host_ed25519"} {
		expected := journal.DBHash
		if name == "host_ed25519" {
			expected = journal.KeyHash
			if expected == "" {
				continue
			}
		}
		target := filepath.Join(state, name)
		if sum, err := digestFile(target); err == nil && sum == expected {
			continue
		}
		install := filepath.Join(stage, "install-"+name)
		if err := os.Remove(install); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := copyPrivate(filepath.Join(stage, name), install); err != nil {
			return err
		}
		if err := operations.replace(install, target); err != nil {
			return err
		}
		if err := operations.syncDirectory(state); err != nil {
			return err
		}
	}
	if err := operations.syncDirectory(state); err != nil {
		return err
	}
	// Durably switch the journal before it can disappear. If an unlink later
	// reappears after a crash, it can only request cleanup, never replay old data.
	journal.Phase = "committed"
	token, err := secretToken()
	if err != nil {
		return err
	}
	commitTemp := filepath.Join(state, ".restore-commit-"+token)
	defer os.Remove(commitTemp)
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err = writeSyncedPrivate(commitTemp, data); err != nil {
		return err
	}
	if err = operations.replace(commitTemp, filepath.Join(state, restoreMarkerName)); err != nil {
		return err
	}
	if err = operations.syncDirectory(state); err != nil {
		return err
	}
	return finishCommittedRestore(state, journal, operations)
}

func finishCommittedRestore(state string, journal restoreJournal, operations restoreOperations) error {
	// A committed journal is valid even after staging cleanup and after the
	// application has subsequently written newer records. Never inspect/replace
	// the target database or key in this phase.
	marker := filepath.Join(state, restoreMarkerName)
	if err := syncPrivateFile(marker); err != nil {
		return err
	}
	if err := operations.syncDirectory(state); err != nil {
		return err
	}
	cleanupRestoreStage(filepath.Join(state, journal.Stage))
	if err := os.Remove(marker); err != nil {
		return err
	}
	return operations.syncDirectory(state)
}

func cleanupRestoreStage(stage string) {
	if !strings.HasPrefix(filepath.Base(stage), ".restore-stage-") {
		return
	}
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	// Only known staging files are removed; cleanup never recursively deletes.
	for _, name := range []string{"miochat.db", "miochat.db-wal", "miochat.db-shm", "host_ed25519", "manifest.json", "install-miochat.db", "install-host_ed25519"} {
		_ = os.Remove(filepath.Join(stage, name))
	}
	_ = os.Remove(stage)
}
