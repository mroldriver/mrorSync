package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temp file in the same directory and renames
// it into place, so a crash never leaves a half-written JSON file behind.
func WriteFileAtomic(name string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(name)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
