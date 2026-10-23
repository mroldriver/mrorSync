package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mrorsync/internal/baidu"
	"mrorsync/internal/config"
	"mrorsync/internal/fsutil"
)

// Version is printed by --version.
var Version = "3.0.9"

// BuildCommit is injected by build.ps1 (-ldflags -X) so --version can be tied to
// a specific commit; it stays empty for plain `go build`.
var BuildCommit = ""

// Options are the CLI switches, shared with main.
type Options struct {
	Apply       bool
	Full        bool
	Verify      bool
	NoDelete    bool
	ForceDelete bool
	Only        string
	Limit       int
	ConfigPath  string
	Out         io.Writer

	// One-shot overrides; empty/zero means "use config file / environment".
	Root         string
	RemoteBase   string
	MaxWorkers   int
	PartWorkers  int
	StateDir     string
	ExcludePaths []string

	PrintConfig bool
	CheckConfig bool
}

// Validate rejects combinations that would surprise the user.
func (o *Options) Validate() error {
	if o.Verify && !o.Apply {
		return errors.New("--verify 需要配合 --apply 使用（会读盘重算，再按需上传）")
	}
	if o.Limit < 0 {
		return errors.New("--limit 需要一个非负整数")
	}
	return nil
}

// Failure is one file that could not be uploaded.
type Failure struct {
	Rel   string `json:"rel"`
	Error string `json:"error"`
}

type stats struct {
	ok, rapid, checked, failed int64
	parts, bytes, deleted      int64
	netBytes                   int64
}

// Engine runs one sync pass.
type Engine struct {
	cfg      *config.Config
	opts     Options
	out      io.Writer
	log      func(format string, args ...any)
	stateDir string
	man      *Manifest
	cli      *baidu.Client

	mu       sync.Mutex
	st       stats
	failures []Failure
}

// Run executes one full sync pass and returns the process exit code:
// 0 success, 1 some files failed, 2 config/usage error, 130 interrupted.
func Run(ctx context.Context, opts Options) int {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	// logf is shared by the engine and the HTTP client; the mutex keeps lines
	// from interleaving when workers log concurrently, and every line carries a
	// timestamp so scheduled-task logs can be correlated.
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		logMu.Lock()
		fmt.Fprintf(opts.Out, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
		logMu.Unlock()
	}

	if err := opts.Validate(); err != nil {
		logf("%v", err)
		return 2
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		logf("%v", err)
		return 2
	}
	// Precedence: CLI > environment (applied by Load) > file > defaults.
	if opts.Root != "" {
		cfg.Root = opts.Root
	}
	if opts.RemoteBase != "" {
		cfg.RemoteBase = opts.RemoteBase
	}
	if opts.MaxWorkers != 0 {
		cfg.MaxWorkers = opts.MaxWorkers
	}
	if opts.PartWorkers != 0 {
		cfg.PartWorkers = opts.PartWorkers
	}
	if opts.StateDir != "" {
		cfg.StateDir = opts.StateDir
	}
	if len(opts.ExcludePaths) > 0 {
		cfg.ExcludePaths = append(cfg.ExcludePaths, opts.ExcludePaths...)
	}
	if err := cfg.Normalize(); err != nil {
		logf("%v", err)
		return 2
	}
	// Print the effective config as pure JSON: warnings would corrupt the output
	// for callers doing `--print-config | jq`.
	if opts.PrintConfig {
		data, err := json.MarshalIndent(cfg.Masked(), "", "  ")
		if err != nil {
			logf("无法序列化配置: %v", err)
			return 2
		}
		fmt.Fprintln(opts.Out, string(data))
		return 0
	}
	for _, w := range cfg.Warnings {
		logf("[警告] %s", w)
	}
	if opts.CheckConfig {
		logf("配置 OK: root=%s remote_base=%s workers=%d/%d max_file_size=%s state_dir=%s excludes=%d",
			cfg.Root, cfg.RemoteBase, cfg.MaxWorkers, cfg.PartWorkers,
			HumanBytes(cfg.MaxFileSize), cfg.EffectiveStateDir(), len(cfg.Excludes))
		return 0
	}
	if opts.Apply && cfg.AccessToken == "" && cfg.RefreshToken == "" {
		logf("access_token 和 refresh_token 都是空的，先运行 mrorsync.exe login 完成授权")
		return 2
	}
	if opts.Apply && cfg.MaxWorkers > 3 {
		logf("[提示] max_workers=%d 偏大，容易被限流(31034)，建议 ≤3", cfg.MaxWorkers)
	}

	stateDir := cfg.EffectiveStateDir()
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		logf("state 目录无效: %v", err)
		return 2
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "blocks"), 0o755); err != nil {
		logf("无法创建 state 目录: %v", err)
		return 2
	}

	lockStale := time.Duration(cfg.Tuning.LockStaleHours) * time.Hour
	lock, err := fsutil.AcquireLock(filepath.Join(stateDir, "run.lock"), lockStale)
	if err != nil {
		var le *fsutil.LockedError
		if errors.As(err, &le) {
			logf("已有实例在运行（PID %d，开始于 %s），本次退出", le.Info.PID, le.Info.Started)
		} else {
			logf("无法创建运行锁: %v", err)
		}
		return 2
	}
	defer func() { _ = lock.Release() }()

	man, err := LoadManifest(filepath.Join(stateDir, "manifest.json"))
	if err != nil {
		logf("%v", err)
		return 2
	}
	defer func() { _ = man.Flush() }()

	cli := baidu.New(cfg)
	cli.Logf = logf

	e := &Engine{cfg: cfg, opts: opts, out: opts.Out, log: logf, stateDir: stateDir, man: man, cli: cli}
	return e.run(ctx, logf)
}

func (e *Engine) logf(format string, args ...any) {
	if e.log != nil {
		e.log(format, args...)
		return
	}
	fmt.Fprintf(e.out, format+"\n", args...)
}

func (e *Engine) run(ctx context.Context, logf func(string, ...any)) int {
	cfg := e.cfg

	logf("扫描 %s ...", cfg.Root)
	scanStart := time.Now()
	scanned, err := Scan(cfg, e.opts.Only, logf)
	if err != nil {
		logf("扫描失败: %v", err)
		return 2
	}
	logf("本地 %d 个文件, 扫描耗时 %.1fs", len(scanned.Entries), time.Since(scanStart).Seconds())
	if scanned.Errors > 0 {
		logf("[警告] %d 个目录/文件读取失败，例如: %s", scanned.Errors, strings.Join(scanned.Samples, ", "))
	}

	seen := make(map[string]bool, len(scanned.Entries))
	todo := make([]todoItem, 0, len(scanned.Entries))
	var todoBytes int64
	for _, ent := range scanned.Entries {
		seen[ent.Rel] = true
		rec, has := e.man.Get(ent.Rel)
		sameMeta := has && rec.Size == ent.Size && sameMetadata(rec, ent)
		if has && sameMeta && rec.State == "ok" && !e.opts.Full && !e.opts.Verify {
			continue
		}
		item := todoItem{entry: ent}
		if has && sameMeta && !e.opts.Full && !e.opts.Verify {
			item.known, item.useKnown = rec.MD5, true
		}
		if has && e.opts.Verify {
			r := rec
			item.verify = &r
		}
		todo = append(todo, item)
		todoBytes += ent.Size
	}

	okRels := e.man.OKRels()
	var gone []string
	if e.opts.Only == "" && !e.opts.NoDelete && scanned.Errors == 0 {
		for rel := range okRels {
			if !seen[rel] {
				gone = append(gone, rel)
			}
		}
		sort.Strings(gone)
	}
	knownCount := len(okRels)
	protectPct := e.cfg.Tuning.DeleteProtectPercent
	protectMin := e.cfg.Tuning.DeleteProtectMin
	if len(gone) > max(protectMin, knownCount*protectPct/100) && !e.opts.ForceDelete {
		logf("[保护] 待删除 %d 个 / 清单共 %d 个, 超过 %d%% 阈值(下限 %d), 本次不删",
			len(gone), knownCount, protectPct, protectMin)
		logf("[保护] 确认这些文件是真删了, 再加 --force-delete 重跑")
		gone = nil
	}
	if e.opts.Limit > 0 && len(gone) > 0 {
		logf("[保护] --limit 模式下跳过远端删除 %d 个", len(gone))
		gone = nil
	}

	// Truncate before logging so the summary matches what will actually run.
	if e.opts.Limit > 0 && len(todo) > e.opts.Limit {
		todo = todo[:e.opts.Limit]
		todoBytes = 0
		for _, it := range todo {
			todoBytes += it.entry.Size
		}
	}
	logf("待上传 %d 个（合计 %s）, 待删除 %d 个", len(todo), HumanBytes(todoBytes), len(gone))

	if !e.opts.Apply {
		for i, it := range todo {
			if i >= 20 {
				logf("  ... 其余 %d 个", len(todo)-20)
				break
			}
			logf("  + %s  (%s)", it.entry.Rel, HumanBytes(it.entry.Size))
		}
		for i, rel := range gone {
			if i >= 20 {
				break
			}
			logf("  - %s", rel)
		}
		logf("dry-run 结束, 加 --apply 才会上传")
		return 0
	}

	start := time.Now()
	var idx int64
	poolErr := runPool(ctx, cfg.MaxWorkers, len(todo), func(ctx context.Context, i int) error {
		item := todo[i]
		n := atomic.AddInt64(&idx, 1)
		var res result
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("内部错误(panic): %v", r)
				}
			}()
			res, err = e.syncOne(ctx, item)
		}()
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			e.addFailure(item.entry.Rel, err)
			e.logf("[%d/%d] 失败 %s: %v", n, len(todo), item.entry.Rel, err)
			if baidu.IsAuthError(err) {
				e.logf("[终止] 认证失败，本轮立即停止；请先运行 mrorsync.exe login 再重试")
				return err
			}
			return nil
		}
		rec := Record{
			Size:    item.entry.Size,
			Mtime:   item.entry.Mtime,
			MtimeMS: &item.entry.MtimeMS,
			MD5:     res.MD5,
			FSID:    res.FSID,
			State:   "ok",
			UTime:   time.Now().Unix(),
		}
		if err := e.man.Put(item.entry.Rel, rec); err != nil {
			e.addFailure(item.entry.Rel, err)
			e.logf("[%d/%d] 失败 %s: %v", n, len(todo), item.entry.Rel, err)
			return nil
		}
		if res.State == "same" {
			e.mu.Lock()
			e.st.checked++
			checked := e.st.checked
			e.mu.Unlock()
			if every := int64(e.cfg.Tuning.VerifyProgressEvery); every > 0 && checked%every == 0 {
				e.logf("校验进度 %d ...", checked)
			}
			return nil
		}
		e.mu.Lock()
		e.st.ok++
		e.st.parts += int64(res.Parts)
		e.st.bytes += item.entry.Size
		e.st.netBytes += res.Bytes
		if res.State == "rapid" {
			e.st.rapid++
		}
		e.mu.Unlock()
		label := "上传"
		if res.State == "rapid" {
			label = "秒传"
		}
		e.logf("[%d/%d] %s %s (%s)", n, len(todo), label, item.entry.Rel, HumanBytes(item.entry.Size))
		return nil
	})
	_ = e.man.Flush()
	if ctx.Err() != nil {
		e.logf("已中断，进度已写入 manifest，下次自动续跑")
		return 130
	}
	if poolErr != nil {
		e.logf("本轮已终止: %v", poolErr)
	}

	if len(gone) > 0 {
		if poolErr != nil {
			e.logf("[跳过] 本轮异常终止，未执行远端删除 %d 个", len(gone))
		} else {
			paths := make([]string, len(gone))
			for i, rel := range gone {
				paths[i] = remoteFor(cfg, rel)
			}
			n, err := e.cli.Delete(ctx, paths)
			if err != nil {
				e.logf("删除失败: %v", err)
			} else if err := e.man.Drop(gone); err != nil {
				e.logf("删除已完成，但 manifest 落盘失败: %v", err)
			} else {
				e.mu.Lock()
				e.st.deleted = int64(n)
				e.mu.Unlock()
				e.logf("远端已删除 %d 个文件", n)
			}
		}
	}

	// A clean run may have left block caches behind for files that were deleted
	// or replaced; drop the unreferenced ones so .state does not grow forever.
	if len(e.failures) == 0 && poolErr == nil {
		age := time.Duration(e.cfg.Tuning.CacheGCHours) * time.Hour
		if n, err := PruneBlockCaches(e.stateDir, e.man.MD5Set(), age); err != nil {
			e.logf("[警告] 清理分片缓存失败: %v", err)
		} else if n > 0 {
			e.logf("已清理 %d 个孤立分片缓存", n)
		}
	}

	cost := time.Since(start).Seconds()
	if cost < 0.001 {
		cost = 0.001
	}
	e.mu.Lock()
	st := e.st
	e.mu.Unlock()
	logf("完成: 成功 %d（秒传 %d，验证一致 %d，上传分片 %d）/ 失败 %d / 删除 %d / 逻辑 %s（实传 %s）/ %.0fs / %s/s",
		st.ok, st.rapid, st.checked, st.parts, st.failed, st.deleted,
		HumanBytes(st.bytes), HumanBytes(st.netBytes), cost, HumanBytes(int64(float64(st.netBytes)/cost)))

	if len(e.failures) > 0 {
		logf("失败清单（前 20 个，manifest 未记成功，下次自动重试）:")
		for i, f := range e.failures {
			if i >= 20 {
				logf("  ... 其余 %d 个", len(e.failures)-20)
				break
			}
			logf("  ! %s -> %s", f.Rel, f.Error)
		}
	}

	writeLastRun(e.stateDir, lastRun{
		At:         time.Now().Format(time.RFC3339),
		Root:       cfg.Root,
		RemoteBase: cfg.RemoteBase,
		Scanned:    len(scanned.Entries),
		Todo:       len(todo),
		OK:         int(st.ok),
		Rapid:      int(st.rapid),
		Checked:    int(st.checked),
		Failed:     int(st.failed),
		Parts:      int(st.parts),
		Deleted:    int(st.deleted),
		Bytes:      st.bytes,
		BytesNet:   st.netBytes,
		Seconds:    float64(int(cost*10)) / 10,
		Failures:   e.failures,
	})

	if st.failed > 0 {
		return 1
	}
	return 0
}

func (e *Engine) addFailure(rel string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.failed++
	e.failures = append(e.failures, Failure{Rel: rel, Error: err.Error()})
}

// runPool runs fn over count items with a bounded number of workers and cancels
// the rest as soon as one item returns an error.
func runPool(ctx context.Context, workers, count int, fn func(context.Context, int) error) error {
	if count == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > count {
		workers = count
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		next     int64 = -1
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				i := int(atomic.AddInt64(&next, 1))
				if i >= count {
					return
				}
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func sameMetadata(rec Record, ent Entry) bool {
	if rec.MtimeMS != nil {
		return *rec.MtimeMS == ent.MtimeMS
	}
	return rec.Mtime == ent.Mtime
}

func remoteFor(cfg *config.Config, rel string) string {
	if cfg.RemoteBase == "/" {
		return "/" + rel
	}
	return cfg.RemoteBase + "/" + rel
}

type lastRun struct {
	At         string    `json:"at"`
	Root       string    `json:"root"`
	RemoteBase string    `json:"remote_base"`
	Scanned    int       `json:"scanned"`
	Todo       int       `json:"todo"`
	OK         int       `json:"ok"`
	Rapid      int       `json:"rapid"`
	Checked    int       `json:"checked"`
	Failed     int       `json:"failed"`
	Parts      int       `json:"parts"`
	Deleted    int       `json:"deleted"`
	Bytes      int64     `json:"bytes"`
	BytesNet   int64     `json:"bytes_net"`
	Seconds    float64   `json:"seconds"`
	Failures   []Failure `json:"failures"`
}

func writeLastRun(stateDir string, lr lastRun) {
	if lr.Failures == nil {
		lr.Failures = []Failure{}
	}
	data, err := json.MarshalIndent(lr, "", "  ")
	if err != nil {
		return
	}
	_ = fsutil.WriteFileAtomic(filepath.Join(stateDir, "last-run.json"), data, 0o644)
}
