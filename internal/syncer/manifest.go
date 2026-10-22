package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"mrorsync/internal/fsutil"
)

// Record is one entry of .state/manifest.json.
type Record struct {
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	MtimeMS *int64 `json:"mtime_ms,omitempty"`
	MD5     string `json:"md5"`
	FSID    int64  `json:"fs_id"`
	State   string `json:"state"`
	UTime   int64  `json:"utime"`
}

// Manifest is the local record of what has been uploaded. Safe for concurrent use.
type Manifest struct {
	file  string
	mu    sync.Mutex
	m     map[string]Record
	dirty int
}

// LoadManifest reads the manifest, tolerating a missing file but failing loudly
// on a corrupt one (silently starting over would re-upload everything).
func LoadManifest(file string) (*Manifest, error) {
	m := &Manifest{file: file, m: map[string]Record{}}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m.m); err != nil {
		return nil, fmt.Errorf("manifest 损坏，无法解析: %s: %w", file, err)
	}
	return m, nil
}

// Get returns the record for rel.
func (m *Manifest) Get(rel string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.m[rel]
	return rec, ok
}

// Put inserts/updates a record and flushes every 200 writes.
func (m *Manifest) Put(rel string, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[rel] = rec
	m.dirty++
	if m.dirty >= 200 {
		return m.flushLocked()
	}
	return nil
}

// Drop removes records and flushes immediately.
func (m *Manifest) Drop(rels []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rels {
		delete(m.m, r)
	}
	m.dirty++
	return m.flushLocked()
}

// OKRels returns all rel paths whose last upload succeeded.
func (m *Manifest) OKRels() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.m))
	for k, v := range m.m {
		if v.State == "ok" {
			out[k] = true
		}
	}
	return out
}

// Len returns the total number of records.
func (m *Manifest) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// Flush writes the manifest if it has pending changes.
func (m *Manifest) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushLocked()
}

func (m *Manifest) flushLocked() error {
	if m.dirty == 0 {
		if _, err := os.Stat(m.file); err == nil {
			return nil
		}
	}
	data, err := json.Marshal(m.m)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(m.file, data, 0o644); err != nil {
		return err
	}
	m.dirty = 0
	return nil
}
