package fsutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// LockInfo is the content of a lock file, useful for telling the user who holds it.
type LockInfo struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
}

// LockedError means another instance holds the lock.
type LockedError struct {
	Info LockInfo
	Age  time.Duration
}

func (e *LockedError) Error() string {
	holder := "未知"
	if e.Info.PID != 0 {
		holder = fmt.Sprintf("PID %d，开始于 %s", e.Info.PID, e.Info.Started)
	}
	return fmt.Sprintf("另一个实例正在运行（%s，锁已存在 %s）", holder, e.Age.Round(time.Second))
}

// Lock is an exclusive, advisory lock backed by a file.
type Lock struct {
	path string
}

// AcquireLock creates path exclusively. A lock older than staleAfter is assumed
// to be left behind by a crashed run and is taken over; staleAfter <= 0 disables
// takeover. The caller must Release the returned lock.
func AcquireLock(path string, staleAfter time.Duration) (*Lock, error) {
	info := LockInfo{PID: os.Getpid(), Started: time.Now().Format(time.RFC3339)}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, werr := f.Write(data); werr != nil {
				f.Close()
				_ = os.Remove(path)
				return nil, werr
			}
			if cerr := f.Close(); cerr != nil {
				_ = os.Remove(path)
				return nil, cerr
			}
			return &Lock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}

		st, serr := os.Stat(path)
		if serr != nil {
			// Vanished between open and stat; try once more.
			continue
		}
		age := time.Since(st.ModTime())
		if staleAfter > 0 && age > staleAfter {
			_ = os.Remove(path)
			continue
		}

		var held LockInfo
		if b, rerr := os.ReadFile(path); rerr == nil {
			_ = json.Unmarshal(b, &held)
		}
		return nil, &LockedError{Info: held, Age: age}
	}

	return nil, &LockedError{}
}

// Release removes the lock file. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
