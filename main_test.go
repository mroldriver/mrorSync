package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type mockFile struct {
	size int64
	fsID int64
	sig  string
}

type mockUpload struct {
	path   string
	size   int64
	blocks []string
}

type mockPart struct {
	path string
	seq  int
}

type mockState struct {
	mu           sync.Mutex
	token        string
	refreshToken string
	dirs         map[string]bool
	files        map[string]*mockFile
	uploads      map[string]*mockUpload
	parts        []mockPart
	deletes      []string
	creates      []string
	precreates   int
	refreshes    int
	busy         int
	seq          int
	createExists bool
}

func newMockState() *mockState {
	return &mockState{
		token:        "t1",
		refreshToken: "r1",
		dirs:         map[string]bool{},
		files:        map[string]*mockFile{},
		uploads:      map[string]*mockUpload{},
	}
}

func (s *mockState) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		ok := q.Get("grant_type") == "refresh_token" && q.Get("refresh_token") == s.refreshToken
		if ok {
			s.refreshes++
			s.token = "t2"
			s.refreshToken = "r2"
		}
		tok, rt := s.token, s.refreshToken
		s.mu.Unlock()
		if !ok {
			writeJSON(w, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, map[string]any{"access_token": tok, "refresh_token": rt, "expires_in": 2592000})
	})

	mux.HandleFunc("/xpan", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, map[string]any{"errno": 31023})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.PostForm.Get("access_token") != s.token {
			writeJSON(w, map[string]any{"errno": -6})
			return
		}
		switch r.PostForm.Get("method") {
		case "precreate":
			s.precreate(w, r)
		case "create":
			s.create(w, r)
		case "filemanager":
			s.delete(w, r)
		default:
			writeJSON(w, map[string]any{"errno": 31023, "request_id": "unknown-method"})
		}
	})

	mux.HandleFunc("/pcs", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Query().Get("access_token") != s.token {
			writeJSON(w, map[string]any{"errno": -6})
			return
		}
		if s.busy > 0 {
			s.busy--
			writeJSON(w, map[string]any{"errno": 31034, "request_id": "busy"})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		var seq int
		fmt.Sscan(r.URL.Query().Get("partseq"), &seq)
		s.parts = append(s.parts, mockPart{path: r.URL.Query().Get("path"), seq: seq})
		writeJSON(w, map[string]any{"errno": 0})
	})
	return mux
}

func (s *mockState) precreate(w http.ResponseWriter, r *http.Request) {
	s.precreates++
	remote := r.PostForm.Get("path")
	var size int64
	fmt.Sscan(r.PostForm.Get("size"), &size)
	var blocks []string
	_ = json.Unmarshal([]byte(r.PostForm.Get("block_list")), &blocks)
	sig := r.PostForm.Get("block_list")

	if prev, ok := s.files[remote]; ok && prev.sig == sig && prev.size == size {
		writeJSON(w, map[string]any{"errno": 0, "return_type": 2, "info": map[string]any{"fs_id": prev.fsID}})
		return
	}
	s.seq++
	id := fmt.Sprintf("u%d", s.seq)
	s.uploads[id] = &mockUpload{path: remote, size: size, blocks: blocks}
	exist := []string{}
	if len(blocks) > 1 {
		exist = blocks[:len(blocks)-1]
	}
	writeJSON(w, map[string]any{"errno": 0, "return_type": 1, "uploadid": id, "block_list": exist})
}

func (s *mockState) create(w http.ResponseWriter, r *http.Request) {
	remote := r.PostForm.Get("path")
	if r.PostForm.Get("isdir") == "1" {
		if s.dirs[remote] {
			writeJSON(w, map[string]any{"errno": -8, "request_id": "dir"})
			return
		}
		s.dirs[remote] = true
		writeJSON(w, map[string]any{"errno": 0, "fs_id": 0})
		return
	}
	uploadID := r.PostForm.Get("uploadid")
	if _, ok := s.uploads[uploadID]; !ok {
		writeJSON(w, map[string]any{"errno": 31066, "request_id": "bad-uploadid"})
		return
	}
	if s.createExists {
		// Simulate the first create succeeding server-side but its response
		// being lost, so the retry sees "already exists".
		s.createExists = false
		writeJSON(w, map[string]any{"errno": 31039, "request_id": "exists"})
		return
	}
	var size int64
	fmt.Sscan(r.PostForm.Get("size"), &size)
	fsID := int64(1000 + len(s.files) + 1)
	s.files[remote] = &mockFile{size: size, fsID: fsID, sig: r.PostForm.Get("block_list")}
	s.creates = append(s.creates, remote)
	delete(s.uploads, uploadID)
	writeJSON(w, map[string]any{"errno": 0, "fs_id": fsID})
}

func (s *mockState) delete(w http.ResponseWriter, r *http.Request) {
	var list []string
	_ = json.Unmarshal([]byte(r.PostForm.Get("filelist")), &list)
	info := make([]map[string]any, 0, len(list))
	for _, remote := range list {
		if _, ok := s.files[remote]; ok {
			delete(s.files, remote)
			s.deletes = append(s.deletes, remote)
			info = append(info, map[string]any{"errno": 0, "path": remote})
		} else {
			info = append(info, map[string]any{"errno": 31064, "path": remote})
		}
	}
	writeJSON(w, map[string]any{"errno": 0, "info": info})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type manifestRec struct {
	Size    int64  `json:"size"`
	Mtime   int64  `json:"mtime"`
	MtimeMS *int64 `json:"mtime_ms"`
	MD5     string `json:"md5"`
	FSID    int64  `json:"fs_id"`
	State   string `json:"state"`
}

type env struct {
	t        *testing.T
	root     string
	stateDir string
	cfgPath  string
	srv      *httptest.Server
	mock     *mockState
}

func setup(t *testing.T) *env {
	t.Helper()
	tmp := t.TempDir()
	e := &env{
		t:        t,
		root:     filepath.Join(tmp, "root"),
		stateDir: filepath.Join(tmp, "state"),
		cfgPath:  filepath.Join(tmp, "config.json"),
		mock:     newMockState(),
	}
	for _, d := range []string{"sub/deep", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(e.root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(e.root, "a.txt"), []byte("hello world"))
	mustWrite(t, filepath.Join(e.root, "sub", "b.bin"), randomBytes(5*1024*1024+123))
	mustWrite(t, filepath.Join(e.root, "sub", "deep", "c.bin"), randomBytes(4*1024*1024))
	mustWrite(t, filepath.Join(e.root, "empty.txt"), nil)
	mustWrite(t, filepath.Join(e.root, "skip.log"), []byte("should be excluded"))
	mustWrite(t, filepath.Join(e.root, "node_modules", "x.js"), []byte("excluded"))
	mustWrite(t, filepath.Join(e.root, "name+(1).txt"), []byte("regex chars in name"))

	cfg := map[string]any{
		"root":          e.root,
		"remote_base":   "/test",
		"app_key":       "k",
		"secret_key":    "s",
		"access_token":  "t1",
		"refresh_token": "r1",
		"max_workers":   2,
		"part_workers":  2,
		"excludes":      []string{"*.log", "node_modules"},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	mustWrite(t, e.cfgPath, data)

	e.srv = httptest.NewServer(e.mock.handler())
	t.Cleanup(e.srv.Close)
	t.Setenv("BAIDU_PAN_API", e.srv.URL+"/xpan")
	t.Setenv("BAIDU_PCS_API", e.srv.URL+"/pcs")
	t.Setenv("BAIDU_OAUTH_API", e.srv.URL+"/oauth")
	t.Setenv("BAIDU_SYNC_STATE_DIR", e.stateDir)
	return e
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (e *env) run(args ...string) (int, string) {
	e.t.Helper()
	var buf safeBuffer
	code := run(context.Background(), args, &buf, strings.NewReader(""))
	return code, buf.String()
}

func (e *env) manifest() map[string]manifestRec {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.stateDir, "manifest.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var m map[string]manifestRec
	if err := json.Unmarshal(data, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestSyncFlow(t *testing.T) {
	e := setup(t)
	apply := []string{"--apply", "--config", e.cfgPath}

	// 1) first sync
	code, out := e.run(apply...)
	if code != 0 {
		t.Fatalf("first apply exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	files, parts, precreates := len(e.mock.files), len(e.mock.parts), e.mock.precreates
	e.mock.mu.Unlock()
	if files != 5 {
		t.Fatalf("want 5 remote files, got %d", files)
	}
	if parts != 4 {
		t.Fatalf("want 4 uploaded parts (block reuse), got %d", parts)
	}
	if precreates != 5 {
		t.Fatalf("want 5 precreate, got %d", precreates)
	}
	if man := e.manifest(); len(man) != 5 {
		t.Fatalf("want 5 manifest entries, got %d", len(man))
	}

	// 2) second run must be a no-op
	code, out = e.run(apply...)
	if code != 0 {
		t.Fatalf("second apply exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	parts2, pre2 := len(e.mock.parts), e.mock.precreates
	e.mock.mu.Unlock()
	if parts2 != parts || pre2 != precreates {
		t.Fatalf("unchanged files hit the network: parts %d->%d precreate %d->%d", parts, parts2, precreates, pre2)
	}

	// 3) --full should rehash everything and hit rapid upload
	code, out = e.run("--apply", "--full", "--config", e.cfgPath)
	if code != 0 {
		t.Fatalf("full exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "秒传") {
		t.Fatalf("expected rapid upload in output:\n%s", out)
	}
	e.mock.mu.Lock()
	creates := len(e.mock.creates)
	e.mock.mu.Unlock()
	if creates != 5 {
		t.Fatalf("rapid upload must not call create, creates=%d", creates)
	}

	// 4) --verify fixes a corrupted manifest md5
	man := e.manifest()
	rec := man["a.txt"]
	rec.MD5 = strings.Repeat("0", 32)
	man["a.txt"] = rec
	data, _ := json.Marshal(man)
	mustWrite(t, filepath.Join(e.stateDir, "manifest.json"), data)

	e.mock.mu.Lock()
	preBefore := e.mock.precreates
	e.mock.mu.Unlock()
	code, out = e.run("--apply", "--verify", "--config", e.cfgPath)
	if code != 0 {
		t.Fatalf("verify exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	preAfter := e.mock.precreates
	e.mock.mu.Unlock()
	if preAfter != preBefore+1 {
		t.Fatalf("verify should precreate only a.txt: %d -> %d", preBefore, preAfter)
	}
	if got := e.manifest()["a.txt"].MD5; got == strings.Repeat("0", 32) {
		t.Fatal("verify did not repair md5")
	}

	// 5) corrupt block caches; a failed record reuses known md5 but must self-heal
	blocksDir := filepath.Join(e.stateDir, "blocks")
	entries, _ := os.ReadDir(blocksDir)
	for _, ent := range entries {
		mustWrite(t, filepath.Join(blocksDir, ent.Name()), []byte("{broken"))
	}
	man = e.manifest()
	rec = man["a.txt"]
	rec.State = "failed"
	man["a.txt"] = rec
	data, _ = json.Marshal(man)
	mustWrite(t, filepath.Join(e.stateDir, "manifest.json"), data)

	code, out = e.run(apply...)
	if code != 0 {
		t.Fatalf("cache self-heal exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "秒传") {
		t.Fatalf("expected rapid upload after cache corruption:\n%s", out)
	}
	valid := 0
	entries, _ = os.ReadDir(blocksDir)
	for _, ent := range entries {
		b, _ := os.ReadFile(filepath.Join(blocksDir, ent.Name()))
		var c struct {
			Blocks []string `json:"blocks"`
		}
		if json.Unmarshal(b, &c) == nil && len(c.Blocks) > 0 {
			valid++
		}
	}
	if valid == 0 {
		t.Fatal("corrupt cache was not rewritten")
	}

	// 6) expired token triggers refresh and retries with the new token
	e.mock.mu.Lock()
	e.mock.token = "expired"
	e.mock.mu.Unlock()
	appendFile(t, filepath.Join(e.root, "a.txt"), []byte("!"))
	code, out = e.run(apply...)
	if code != 0 {
		t.Fatalf("token refresh exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	refreshes := e.mock.refreshes
	e.mock.mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("want 1 refresh, got %d\n%s", refreshes, out)
	}
	var cfg map[string]any
	cfgData, _ := os.ReadFile(e.cfgPath)
	_ = json.Unmarshal(cfgData, &cfg)
	if cfg["access_token"] != "t2" {
		t.Fatalf("token not persisted, got %v", cfg["access_token"])
	}

	// 7) deleting a local file prunes the remote
	if err := os.Remove(filepath.Join(e.root, "empty.txt")); err != nil {
		t.Fatal(err)
	}
	code, out = e.run(apply...)
	if code != 0 {
		t.Fatalf("delete exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	deletes := append([]string(nil), e.mock.deletes...)
	e.mock.mu.Unlock()
	if len(deletes) != 1 || deletes[0] != "/test/empty.txt" {
		t.Fatalf("unexpected deletes: %v", deletes)
	}
	if got := len(e.manifest()); got != 4 {
		t.Fatalf("manifest after delete = %d, want 4", got)
	}

	// 8) --limit must never delete
	if err := os.Remove(filepath.Join(e.root, "sub", "deep", "c.bin")); err != nil {
		t.Fatal(err)
	}
	code, out = e.run("--apply", "--limit", "1", "--config", e.cfgPath)
	if code != 0 {
		t.Fatalf("limit exit=%d\n%s", code, out)
	}
	e.mock.mu.Lock()
	deletesAfter := len(e.mock.deletes)
	e.mock.mu.Unlock()
	if deletesAfter != len(deletes) {
		t.Fatal("--limit deleted remote files")
	}
	if _, ok := e.manifest()["sub/deep/c.bin"]; !ok {
		t.Fatal("--limit dropped the manifest entry")
	}

	// 9) 31034 busy is retried with backoff
	e.mock.mu.Lock()
	e.mock.busy = 1
	e.mock.mu.Unlock()
	mustWrite(t, filepath.Join(e.root, "sub", "b.bin"), randomBytes(5*1024*1024+123))
	code, out = e.run(apply...)
	if code != 0 {
		t.Fatalf("busy retry exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "完成") {
		t.Fatalf("expected summary:\n%s", out)
	}

	// 10) --only matches case-insensitively and stays inside the subtree
	code, out = e.run("--only", "SUB", "--config", e.cfgPath)
	if code != 0 {
		t.Fatalf("--only exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "本地 1 个文件") {
		t.Fatalf("--only SUB should scan exactly 1 file, got:\n%s", out)
	}

	// 11) --verify without --apply is rejected
	code, _ = e.run("--verify", "--config", e.cfgPath)
	if code != 2 {
		t.Fatalf("--verify alone exit=%d, want 2", code)
	}

	var last struct {
		Failed int `json:"failed"`
	}
	lastData, err := os.ReadFile(filepath.Join(e.stateDir, "last-run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lastData, &last); err != nil {
		t.Fatal(err)
	}
	if last.Failed != 0 {
		t.Fatalf("last-run failed=%d", last.Failed)
	}
}

func TestCreateAlreadyExistsIsSuccess(t *testing.T) {
	e := setup(t)
	e.mock.mu.Lock()
	e.mock.createExists = true
	e.mock.mu.Unlock()

	code, out := e.run("--apply", "--config", e.cfgPath)
	if code != 0 {
		t.Fatalf("create-already-exists exit=%d\n%s", code, out)
	}
	if !strings.Contains(out, "已存在") {
		t.Fatalf("expected an already-exists hint, got:\n%s", out)
	}
	man := e.manifest()
	if len(man) != 5 {
		t.Fatalf("want 5 manifest entries, got %d", len(man))
	}
	for rel, rec := range man {
		if rec.State != "ok" {
			t.Fatalf("%s state=%q, want ok", rel, rec.State)
		}
	}
	// The lock must be gone once the run returns.
	if _, err := os.Stat(filepath.Join(e.stateDir, "run.lock")); !os.IsNotExist(err) {
		t.Fatalf("run.lock left behind: %v", err)
	}
}

func TestAuthFailureStopsRun(t *testing.T) {
	e := setup(t)

	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["access_token"] = "bad"
	cfg["refresh_token"] = "bad"
	data, _ = json.MarshalIndent(cfg, "", "  ")
	mustWrite(t, e.cfgPath, data)

	code, out := e.run("--apply", "--config", e.cfgPath)
	if code != 1 {
		t.Fatalf("auth failure exit=%d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "认证失败") {
		t.Fatalf("expected auth failure message:\n%s", out)
	}
	e.mock.mu.Lock()
	precreates := e.mock.precreates
	creates := len(e.mock.creates)
	e.mock.mu.Unlock()
	if precreates >= 5 {
		t.Fatalf("auth failure did not stop the run: precreate=%d", precreates)
	}
	if creates != 0 {
		t.Fatalf("auth failure still created %d files", creates)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}
