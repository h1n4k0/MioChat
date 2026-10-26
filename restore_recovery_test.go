package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func restoreFixture(t *testing.T, name string) string {
	t.Helper()
	state := t.TempDir()
	s, err := openStore(filepath.Join(state, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Register(name, "restore-password-123", "restore-password-123"); err != nil {
		t.Fatal(err)
	}
	if _, err = hostSigner(filepath.Join(state, "host_ed25519")); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	return state
}

func TestRestoreInterruptedKeyInstallRecoversBeforeOpening(t *testing.T) {
	source := restoreFixture(t, "source-user")
	backup := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := MakeSnapshot(source, backup)
	if err != nil {
		t.Fatal(err)
	}
	target := restoreFixture(t, "target-user")
	oldKey, _ := digestFile(filepath.Join(target, "host_ed25519"))
	if oldKey == manifest.HostKeySHA256 {
		t.Fatal("test generations unexpectedly share a key")
	}
	failed := false
	err = restoreSnapshotWithOps(target, backup, restoreOperations{
		replace: func(source, destination string) error {
			if filepath.Base(destination) == "host_ed25519" {
				failed = true
				return os.ErrPermission
			}
			return replaceFile(source, destination)
		},
		syncDirectory: syncDirectory,
	})
	if err == nil || !failed {
		t.Fatal("second install phase did not fail", err)
	}
	if err = ensureNoPendingRestore(target); err == nil {
		t.Fatal("mixed recovery generation was not blocked")
	}
	if s, err := openStore(filepath.Join(target, "miochat.db")); err == nil {
		s.db.Close()
		t.Fatal("database opened before pending recovery completed")
	}
	if _, err = MakeSnapshot(target, filepath.Join(t.TempDir(), "not-a-consistent-generation")); err == nil {
		t.Fatal("backup accepted an incomplete recovery")
	}
	// Journals written before phase tracking are installing journals, not a
	// reason to skip an incomplete pair.
	markerData, err := os.ReadFile(filepath.Join(target, restoreMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var legacy restoreJournal
	if err = json.Unmarshal(markerData, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Phase = ""
	markerData, _ = json.Marshal(legacy)
	if err = os.WriteFile(filepath.Join(target, restoreMarkerName), markerData, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := lockState(target)
	if err != nil {
		t.Fatal("exclusive startup could not finish recovery", err)
	}
	release()
	if err = ensureNoPendingRestore(target); err != nil {
		t.Fatal(err)
	}
	dbHash, _ := digestFile(filepath.Join(target, "miochat.db"))
	keyHash, _ := digestFile(filepath.Join(target, "host_ed25519"))
	if dbHash != manifest.DatabaseSHA256 || keyHash != manifest.HostKeySHA256 {
		t.Fatal("recovery did not install the complete requested generation")
	}
	rescues, _ := filepath.Glob(filepath.Join(target, "restore-rescue-*"))
	if len(rescues) != 1 {
		t.Fatal("original generation was not preserved")
	}
	rescue, err := ValidateSnapshot(rescues[0])
	if err != nil || rescue.HostKeySHA256 != oldKey {
		t.Fatal("original identity was not preserved", err)
	}
	// Recovery after completion is an idempotent no-op.
	if err = recoverPendingRestore(target); err != nil {
		t.Fatal(err)
	}
}

func TestRestorePrevalidatesKeyFingerprintAndKeepsTarget(t *testing.T) {
	source := restoreFixture(t, "source-user")
	backup := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := MakeSnapshot(source, backup)
	if err != nil {
		t.Fatal(err)
	}
	target := restoreFixture(t, "target-user")
	beforeDB, _ := digestFile(filepath.Join(target, "miochat.db"))
	beforeKey, _ := digestFile(filepath.Join(target, "host_ed25519"))
	manifest.HostFingerprint = "SHA256:not-the-backed-up-identity"
	data, _ := json.Marshal(manifest)
	if err = os.WriteFile(filepath.Join(backup, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = RestoreSnapshot(target, backup); err == nil {
		t.Fatal("restore accepted a forged host fingerprint")
	}
	afterDB, _ := digestFile(filepath.Join(target, "miochat.db"))
	afterKey, _ := digestFile(filepath.Join(target, "host_ed25519"))
	if beforeDB != afterDB || beforeKey != afterKey {
		t.Fatal("invalid backup changed the target")
	}
}

func TestRestoreRejectsTamperedPendingStage(t *testing.T) {
	source := restoreFixture(t, "source-user")
	backup := filepath.Join(t.TempDir(), "snapshot")
	if _, err := MakeSnapshot(source, backup); err != nil {
		t.Fatal(err)
	}
	target := restoreFixture(t, "target-user")
	err := restoreSnapshotWithOps(target, backup, restoreOperations{
		replace: func(source, destination string) error {
			if filepath.Base(destination) == "host_ed25519" {
				return os.ErrPermission
			}
			return replaceFile(source, destination)
		},
		syncDirectory: syncDirectory,
	})
	if err == nil {
		t.Fatal("expected interrupted restore")
	}
	data, err := os.ReadFile(filepath.Join(target, restoreMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var journal restoreJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(target, journal.Stage, "miochat.db"), []byte("invalid staged database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = lockState(target); err == nil {
		t.Fatal("startup accepted a damaged staged generation")
	}
	if err = ensureNoPendingRestore(target); err == nil {
		t.Fatal("failed recovery removed its commit marker")
	}
}

func TestRestoreJournalCannotEscapeTheStateDirectory(t *testing.T) {
	for _, stage := range []string{"../outside", "not-a-restore-stage", ".restore-stage-token/../../outside", filepath.Join(t.TempDir(), ".restore-stage-external")} {
		t.Run(stage, func(t *testing.T) {
			state := restoreFixture(t, "target-user")
			beforeDB, _ := digestFile(filepath.Join(state, "miochat.db"))
			beforeKey, _ := digestFile(filepath.Join(state, "host_ed25519"))
			data, _ := json.Marshal(restoreJournal{Stage: stage})
			if err := os.WriteFile(filepath.Join(state, restoreMarkerName), data, 0600); err != nil {
				t.Fatal(err)
			}
			if release, err := lockState(state); err == nil {
				release()
				t.Fatal("invalid recovery journal was accepted")
			}
			afterDB, _ := digestFile(filepath.Join(state, "miochat.db"))
			afterKey, _ := digestFile(filepath.Join(state, "host_ed25519"))
			if beforeDB != afterDB || beforeKey != afterKey {
				t.Fatal("invalid journal changed the current generation")
			}
		})
	}
}

func TestCommittedRestoreJournalCannotReplayOverNewerData(t *testing.T) {
	source := restoreFixture(t, "source-user")
	backup := filepath.Join(t.TempDir(), "snapshot")
	if _, err := MakeSnapshot(source, backup); err != nil {
		t.Fatal(err)
	}
	target := restoreFixture(t, "target-user")
	var committedMarker []byte
	cleanupFailed := false
	err := restoreSnapshotWithOps(target, backup, restoreOperations{
		replace: replaceFile,
		syncDirectory: func(path string) error {
			if path == target {
				data, readErr := os.ReadFile(filepath.Join(target, restoreMarkerName))
				if readErr == nil {
					var journal restoreJournal
					if json.Unmarshal(data, &journal) == nil && journal.Phase == "committed" {
						committedMarker = append([]byte(nil), data...)
					}
				} else if errors.Is(readErr, os.ErrNotExist) && len(committedMarker) != 0 && !cleanupFailed {
					cleanupFailed = true
					return errors.New("synthetic directory sync failure after marker unlink")
				}
			}
			return syncDirectory(path)
		},
	})
	if err == nil || !cleanupFailed || len(committedMarker) == 0 {
		t.Fatal("test did not fail the final marker cleanup", err)
	}
	if err = ensureNoPendingRestore(target); err != nil {
		t.Fatal("marker was not physically unlinked before the injected failure", err)
	}
	s, err := openStore(filepath.Join(target, "miochat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	message := mustSend(t, s, "source-user", "lobby", "", "new data after completed restore")
	walPath := filepath.Join(target, "miochat.db-wal")
	walBefore, err := digestFile(walPath)
	if err != nil {
		t.Fatal("test requires committed data in the live WAL", err)
	}
	// Simulate metadata reappearing after an unsuccessful directory flush. Even
	// a complete old staging snapshot must not cause the new data to be replayed.
	var journal restoreJournal
	if err = json.Unmarshal(committedMarker, &journal); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(target, journal.Stage)
	if err = os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"miochat.db", "manifest.json", "host_ed25519"} {
		if err = copyPrivate(filepath.Join(backup, name), filepath.Join(stage, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(target, restoreMarkerName), committedMarker, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := lockState(target)
	if err != nil {
		t.Fatal("committed journal could not finish cleanup", err)
	}
	release()
	if walAfter, err := digestFile(walPath); err != nil || walAfter != walBefore {
		t.Fatal("committed recovery touched a newer WAL", err)
	}
	var body string
	if err = s.db.QueryRow(`SELECT body FROM messages WHERE id=?`, message.ID).Scan(&body); err != nil || body != message.Body {
		t.Fatal("committed marker replay overwrote post-restore data", err)
	}
	if err = ensureNoPendingRestore(target); err != nil {
		t.Fatal(err)
	}
	rescues, _ := filepath.Glob(filepath.Join(target, "restore-rescue-*"))
	if len(rescues) != 1 {
		t.Fatal("cleanup removed the prior rescue")
	}
}

func TestRestoreRejectsUnknownJournalPhase(t *testing.T) {
	state := restoreFixture(t, "target-user")
	before, _ := digestFile(filepath.Join(state, "miochat.db"))
	data, _ := json.Marshal(restoreJournal{Stage: ".restore-stage-invalid-phase", Phase: "future-unknown-phase"})
	if err := os.WriteFile(filepath.Join(state, restoreMarkerName), data, 0600); err != nil {
		t.Fatal(err)
	}
	if release, err := lockState(state); err == nil {
		release()
		t.Fatal("unknown journal phase was accepted")
	}
	if after, _ := digestFile(filepath.Join(state, "miochat.db")); after != before {
		t.Fatal("unknown phase changed the database")
	}
}
