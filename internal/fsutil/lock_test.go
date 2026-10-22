package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")

	l1, err := AcquireLock(path, time.Hour)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	_, err = AcquireLock(path, time.Hour)
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("second acquire: want LockedError, got %v", err)
	}
	if le.Info.PID != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", le.Info.PID, os.Getpid())
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	l2, err := AcquireLock(path, time.Hour)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock file left behind: %v", err)
	}
}

func TestAcquireLockTakesOverStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(path, []byte(`{"pid":1,"started":"2000-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	l, err := AcquireLock(path, time.Hour)
	if err != nil {
		t.Fatalf("stale lock should be taken over: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireLockNoTakeoverWhenDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	if err := os.WriteFile(path, []byte(`{"pid":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	_, err := AcquireLock(path, 0)
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("staleAfter=0 must never take over, got %v", err)
	}
}

func TestLockReleaseIsIdempotent(t *testing.T) {
	l := &Lock{}
	if err := l.Release(); err != nil {
		t.Fatalf("nil lock release: %v", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("nil pointer release: %v", err)
	}
}
