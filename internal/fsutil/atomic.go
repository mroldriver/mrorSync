package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temp file in the same directory and renames
// it into place, so a crash never leaves a half-written JSON file behind.
func WriteFileAtomic(name string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(name, data, perm, true)
}

// WriteFileFast is WriteFileAtomic without fsync. Use it for data that can be
// rebuilt from the filesystem (e.g. the block-hash cache): a lost file only
// costs a re-read, while an fsync per file is expensive on a first full sync.
func WriteFileFast(name string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(name, data, perm, false)
}

func writeFileAtomic(name string, data []byte, perm os.FileMode, sync bool) error {
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
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}
