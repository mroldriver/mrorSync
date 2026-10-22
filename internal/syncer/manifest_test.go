package syncer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestManifestFlushRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		rel := fmt.Sprintf("dir/file-%04d.txt", i)
		if err := m.Put(rel, Record{Size: int64(i), State: "ok", MD5: fmt.Sprintf("%032d", i)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Len(); got != 1500 {
		t.Fatalf("after flush len=%d, want 1500", got)
	}
	if err := reloaded.Drop([]string{"dir/file-0000.txt", "dir/file-0001.txt"}); err != nil {
		t.Fatal(err)
	}
	again, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Len(); got != 1498 {
		t.Fatalf("after drop len=%d, want 1498", got)
	}
}

func TestManifestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Fatal("corrupt manifest must fail loudly")
	}
}

func TestManifestConcurrentPutAndFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 4, 600
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				rel := fmt.Sprintf("w%d/f%04d", w, i)
				if err := m.Put(rel, Record{Size: int64(i), State: "ok", MD5: "x"}); err != nil {
					t.Errorf("put %s: %v", rel, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Len(); got != workers*perWorker {
		t.Fatalf("len=%d, want %d", got, workers*perWorker)
	}
}

func TestPruneBlockCaches(t *testing.T) {
	stateDir := t.TempDir()
	blocks := filepath.Join(stateDir, "blocks")
	if err := os.MkdirAll(blocks, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	write := func(name string, mtime time.Time) {
		p := filepath.Join(blocks, name)
		if err := os.WriteFile(p, []byte(`{"blocks":["x"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("aaa.json", old)         // orphan -> removed
	write("bbb.json", old)         // referenced -> kept
	write("ccc.json", time.Now())  // orphan but fresh -> kept
	write("ddd.json.123.tmp", old) // crashed temp -> removed
	write("notes.txt", old)        // not a cache file -> ignored

	removed, err := PruneBlockCaches(stateDir, map[string]bool{"bbb": true}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	for _, name := range []string{"aaa.json", "ddd.json.123.tmp"} {
		if _, err := os.Stat(filepath.Join(blocks, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed", name)
		}
	}
	for _, name := range []string{"bbb.json", "ccc.json", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(blocks, name)); err != nil {
			t.Fatalf("%s should be kept: %v", name, err)
		}
	}
}

func TestManifestMD5Set(t *testing.T) {
	m, _ := LoadManifest(filepath.Join(t.TempDir(), "manifest.json"))
	_ = m.Put("a", Record{MD5: "m1", State: "ok"})
	_ = m.Put("b", Record{MD5: "m2", State: "failed"})
	_ = m.Put("c", Record{State: "ok"})
	set := m.MD5Set()
	if len(set) != 2 || !set["m1"] || !set["m2"] {
		t.Fatalf("md5 set = %v", set)
	}
	_ = m.Flush()
}

func TestManifestFileMissingWritesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	m, _ := LoadManifest(path)
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]Record
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("empty manifest not valid JSON: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("want empty map, got %v", raw)
	}
}
