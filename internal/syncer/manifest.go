package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"mrorsync/internal/fsutil"
)

// flushEvery is how many Put calls may accumulate before the whole map is
// rewritten. Larger batches cut the O(n²) total write volume; on a crash at most
// this many records are lost, and those files are re-uploaded as 秒传 hits.
const flushEvery = 1000

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

	// flushMu serialises writers so two goroutines never rename onto the same
	// path at once; it is deliberately not held while records are being changed.
	flushMu sync.Mutex
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

// Put inserts/updates a record and flushes once a batch has accumulated.
func (m *Manifest) Put(rel string, rec Record) error {
	m.mu.Lock()
	m.m[rel] = rec
	m.dirty++
	need := m.dirty >= flushEvery
	m.mu.Unlock()
	if need {
		return m.flush()
	}
	return nil
}

// Drop removes records and flushes immediately.
func (m *Manifest) Drop(rels []string) error {
	m.mu.Lock()
	for _, r := range rels {
		delete(m.m, r)
	}
	m.dirty++
	m.mu.Unlock()
	return m.flush()
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

// MD5Set returns every whole-file md5 referenced by the manifest.
func (m *Manifest) MD5Set() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.m))
	for _, v := range m.m {
		if v.MD5 != "" {
			out[v.MD5] = true
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
	return m.flush()
}

func (m *Manifest) flush() error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	m.mu.Lock()
	if m.dirty == 0 {
		_, statErr := os.Stat(m.file)
		m.mu.Unlock()
		if statErr == nil {
			return nil
		}
		// No file yet: write an empty manifest so readers can rely on it.
		return m.write(map[string]Record{})
	}
	// Snapshot under the lock, then serialise/write without blocking Put.
	snap := make(map[string]Record, len(m.m))
	for k, v := range m.m {
		snap[k] = v
	}
	m.dirty = 0
	m.mu.Unlock()

	if err := m.write(snap); err != nil {
		m.mu.Lock()
		if m.dirty == 0 {
			m.dirty = 1 // make the next Flush retry the write
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *Manifest) write(snap map[string]Record) error {
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(m.file, data, 0o644)
}
