package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mrorsync/internal/fsutil"
)

const (
	DefaultMaxWorkers  = 2
	DefaultPartWorkers = 2
	DefaultMaxFileSize = int64(4 * 1024 * 1024 * 1024) // 4GB, 普通用户上限

	DefaultRetryTries            = 5
	DefaultBackoffMaxSeconds     = 8
	DefaultBusyBackoffMaxSeconds = 40
	DefaultHTTPTimeoutSeconds    = 180
	DefaultDeleteProtectPercent  = 20
	DefaultDeleteProtectMin      = 20
	DefaultLockStaleHours        = 24
	DefaultCacheGCHours          = 24
	DefaultVerifyProgressEvery   = 2000

	// CurrentVersion is the config schema version this binary understands.
	CurrentVersion = 1

	configFilePerm      = 0o600
	defaultRemoteParent = "/备份/D盘"
)

// Tuning holds operational knobs that used to be hard-coded. Every field has a
// default, so an old config.json without the section keeps working unchanged.
type Tuning struct {
	RetryTries            int `json:"retry_tries"`
	BackoffMaxSeconds     int `json:"backoff_max_seconds"`
	BusyBackoffMaxSeconds int `json:"busy_backoff_max_seconds"`
	HTTPTimeoutSeconds    int `json:"http_timeout_seconds"`
	DeleteProtectPercent  int `json:"delete_protect_percent"`
	DeleteProtectMin      int `json:"delete_protect_min"`
	LockStaleHours        int `json:"lock_stale_hours"`
	CacheGCHours          int `json:"cache_gc_hours"`
	VerifyProgressEvery   int `json:"verify_progress_every"`
}

// Config mirrors config.json one-for-one, so configs written by older versions
// keep working without migration.
type Config struct {
	ConfigVersion int      `json:"config_version"`
	Root          string   `json:"root"`
	RemoteBase    string   `json:"remote_base"`
	AppID         string   `json:"app_id"`
	AppKey        string   `json:"app_key"`
	SecretKey     string   `json:"secret_key"`
	SignKey       string   `json:"sign_key"`
	AccessToken   string   `json:"access_token"`
	RefreshToken  string   `json:"refresh_token"`
	MaxWorkers    int      `json:"max_workers"`
	PartWorkers   int      `json:"part_workers"`
	MaxFileSize   int64    `json:"max_file_size"`
	StateDir      string   `json:"state_dir"`
	Excludes      []string `json:"excludes"`
	// ExcludePaths are relative paths (dir or file) under root to skip entirely;
	// unlike Excludes (base-name matching) they target one specific subtree.
	ExcludePaths []string `json:"exclude_paths"`
	Tuning       Tuning   `json:"tuning"`

	// Warnings collects non-fatal adjustments made by Normalize (clamped values,
	// ignored fields) so the caller can show them instead of silently changing
	// what the user wrote.
	Warnings []string `json:"-"`

	path string
	raw  map[string]json.RawMessage
}

// Default returns the values used when config.json omits a key.
func Default() *Config {
	return &Config{
		ConfigVersion: CurrentVersion,
		Root:          "D:\\",
		RemoteBase:    defaultRemoteParent,
		MaxWorkers:    DefaultMaxWorkers,
		PartWorkers:   DefaultPartWorkers,
		MaxFileSize:   DefaultMaxFileSize,
		Excludes: []string{
			"$RECYCLE.BIN", "System Volume Information", "node_modules", ".git", ".state",
		},
		Tuning: defaultTuning(),
	}
}

func defaultTuning() Tuning {
	return Tuning{
		RetryTries:            DefaultRetryTries,
		BackoffMaxSeconds:     DefaultBackoffMaxSeconds,
		BusyBackoffMaxSeconds: DefaultBusyBackoffMaxSeconds,
		HTTPTimeoutSeconds:    DefaultHTTPTimeoutSeconds,
		DeleteProtectPercent:  DefaultDeleteProtectPercent,
		DeleteProtectMin:      DefaultDeleteProtectMin,
		LockStaleHours:        DefaultLockStaleHours,
		CacheGCHours:          DefaultCacheGCHours,
		VerifyProgressEvery:   DefaultVerifyProgressEvery,
	}
}

// ExeDir returns the directory of the running binary; config/state default to it
// so a scheduled task behaves the same no matter what the working directory is.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// DefaultPath is the config path used when --config is not given.
func DefaultPath() string {
	if v := strings.TrimSpace(os.Getenv("MRORSYNC_CONFIG")); v != "" {
		return v
	}
	return filepath.Join(ExeDir(), "config.json")
}

// Load reads config.json, overlays MRORSYNC_* environment variables, and keeps
// unknown fields so a save (token refresh) does not drop keys added by future
// versions. Precedence: CLI flags > environment > file > defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("缺少配置文件: %s（复制 config.example.json 改名）: %w", path, err)
	}
	c := Default()
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("配置文件不是合法 JSON: %s: %w", path, err)
	}
	c.path = path
	c.raw = map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &c.raw)
	if err := applyEnv(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// EffectiveStateDir resolves the state directory with the documented precedence:
// config.state_dir > MRORSYNC_STATE_DIR > BAIDU_SYNC_STATE_DIR > <exe>/.state.
func (c *Config) EffectiveStateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	for _, key := range []string{"MRORSYNC_STATE_DIR", "BAIDU_SYNC_STATE_DIR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return filepath.Join(ExeDir(), ".state")
}

// Masked returns a deep-enough copy safe to print: secrets are replaced.
func (c *Config) Masked() *Config {
	out := *c
	out.Warnings = nil
	out.raw = nil
	if out.SecretKey != "" {
		out.SecretKey = "******"
	}
	if out.SignKey != "" {
		out.SignKey = "******"
	}
	if out.AccessToken != "" {
		out.AccessToken = "******"
	}
	if out.RefreshToken != "" {
		out.RefreshToken = "******"
	}
	if out.Excludes != nil {
		out.Excludes = append([]string(nil), c.Excludes...)
	}
	return &out
}

// Normalize validates and canonicalises paths and limits. Adjustments that change
// user input are recorded in Warnings.
func (c *Config) Normalize() error {
	c.Warnings = nil

	switch {
	case c.ConfigVersion == 0:
		c.ConfigVersion = CurrentVersion
	case c.ConfigVersion < 0 || c.ConfigVersion > CurrentVersion:
		return fmt.Errorf("config_version=%d 不受支持（当前支持 1）", c.ConfigVersion)
	}

	if strings.TrimSpace(c.Root) == "" {
		return fmt.Errorf("config.root 不能为空")
	}
	root, err := filepath.Abs(filepath.FromSlash(strings.ReplaceAll(c.Root, "\\", "/")))
	if err != nil {
		return fmt.Errorf("root 路径无效: %w", err)
	}
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("root 不存在或不可访问: %s", root)
	}
	if !st.IsDir() {
		return fmt.Errorf("root 不是目录: %s", root)
	}
	c.Root = root

	c.RemoteBase = NormalizeRemoteBase(c.RemoteBase)
	c.MaxWorkers = c.clampWarn("max_workers", c.MaxWorkers, DefaultMaxWorkers, 1, 16)
	c.PartWorkers = c.clampWarn("part_workers", c.PartWorkers, DefaultPartWorkers, 1, 8)
	if c.MaxFileSize <= 0 {
		c.MaxFileSize = DefaultMaxFileSize
	}
	if c.Excludes == nil {
		c.Excludes = Default().Excludes
	}
	for i := range c.Excludes {
		c.Excludes[i] = strings.TrimSpace(c.Excludes[i])
	}
	c.ExcludePaths = normalizeExcludePaths(c.Root, c.ExcludePaths, &c.Warnings)
	if c.StateDir != "" {
		abs, err := filepath.Abs(c.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir 路径无效: %w", err)
		}
		c.StateDir = abs
	}

	t := &c.Tuning
	t.RetryTries = c.clampWarn("tuning.retry_tries", t.RetryTries, DefaultRetryTries, 1, 10)
	t.BackoffMaxSeconds = c.clampWarn("tuning.backoff_max_seconds", t.BackoffMaxSeconds, DefaultBackoffMaxSeconds, 1, 300)
	t.BusyBackoffMaxSeconds = c.clampWarn("tuning.busy_backoff_max_seconds", t.BusyBackoffMaxSeconds, DefaultBusyBackoffMaxSeconds, 1, 600)
	t.HTTPTimeoutSeconds = c.clampWarn("tuning.http_timeout_seconds", t.HTTPTimeoutSeconds, DefaultHTTPTimeoutSeconds, 5, 3600)
	t.DeleteProtectPercent = c.clampWarn("tuning.delete_protect_percent", t.DeleteProtectPercent, DefaultDeleteProtectPercent, 1, 100)
	t.DeleteProtectMin = c.clampWarn("tuning.delete_protect_min", t.DeleteProtectMin, DefaultDeleteProtectMin, 0, 1_000_000)
	t.LockStaleHours = c.clampWarn("tuning.lock_stale_hours", t.LockStaleHours, DefaultLockStaleHours, 1, 720)
	t.CacheGCHours = c.clampWarn("tuning.cache_gc_hours", t.CacheGCHours, DefaultCacheGCHours, 1, 8760)
	t.VerifyProgressEvery = c.clampWarn("tuning.verify_progress_every", t.VerifyProgressEvery, DefaultVerifyProgressEvery, 0, 1_000_000)

	return nil
}

func (c *Config) clampWarn(name string, v, dflt, min, max int) int {
	if v == 0 {
		return dflt
	}
	if v < min || v > max {
		c.Warnings = append(c.Warnings, fmt.Sprintf("%s=%d 超出范围 [%d,%d]，已调整为 %d", name, v, min, max, clamp(v, dflt, min, max)))
	}
	return clamp(v, dflt, min, max)
}

// normalizeExcludePaths turns entries into relative paths with '/' separators.
// Absolute paths under root are converted; absolute paths outside root are
// ignored with a warning instead of silently excluding nothing.
func normalizeExcludePaths(root string, in []string, warnings *[]string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		p := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		if p == "" {
			continue
		}
		if filepath.IsAbs(filepath.FromSlash(p)) {
			rel, err := filepath.Rel(root, filepath.FromSlash(p))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				*warnings = append(*warnings, fmt.Sprintf("exclude_paths 项 %q 不在 root 下，已忽略", raw))
				continue
			}
			p = filepath.ToSlash(rel)
		}
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// NormalizeRemoteBase turns "/备份/D盘/" or "备份\D盘" into "/备份/D盘"; "/" stays "/".
func NormalizeRemoteBase(raw string) string {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
	if s == "" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "/"
	}
	return s
}

// Save writes the config atomically, keeping unknown keys from the original file.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config path 为空")
	}
	known, err := json.Marshal(c)
	if err != nil {
		return err
	}
	merged := map[string]json.RawMessage{}
	for k, v := range c.raw {
		merged[k] = v
	}
	var km map[string]json.RawMessage
	if err := json.Unmarshal(known, &km); err != nil {
		return err
	}
	for k, v := range km {
		merged[k] = v
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return fsutil.WriteFileAtomic(c.path, out, configFilePerm)
}

func clamp(v, dflt, min, max int) int {
	if v == 0 {
		return dflt
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// ---------------------------------------------------------------- environment

// applyEnv overlays MRORSYNC_* variables on top of the file. Invalid numbers are
// reported instead of silently ignored.
func applyEnv(c *Config) error {
	setStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = strings.TrimSpace(v)
		}
	}
	setInt := func(key string, dst *int) error {
		v, ok := os.LookupEnv(key)
		if !ok || strings.TrimSpace(v) == "" {
			return nil
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("环境变量 %s=%q 不是整数", key, v)
		}
		*dst = n
		return nil
	}

	setStr("MRORSYNC_ROOT", &c.Root)
	setStr("MRORSYNC_REMOTE_BASE", &c.RemoteBase)
	setStr("MRORSYNC_APP_ID", &c.AppID)
	setStr("MRORSYNC_APP_KEY", &c.AppKey)
	setStr("MRORSYNC_SECRET_KEY", &c.SecretKey)
	setStr("MRORSYNC_SIGN_KEY", &c.SignKey)
	setStr("MRORSYNC_ACCESS_TOKEN", &c.AccessToken)
	setStr("MRORSYNC_REFRESH_TOKEN", &c.RefreshToken)
	setStr("MRORSYNC_STATE_DIR", &c.StateDir)
	if v, ok := os.LookupEnv("MRORSYNC_EXCLUDES"); ok {
		c.Excludes = splitList(v)
	}
	if v, ok := os.LookupEnv("MRORSYNC_EXCLUDE_PATHS"); ok {
		c.ExcludePaths = splitList(v)
	}

	ints := []struct {
		key string
		dst *int
	}{
		{"MRORSYNC_MAX_WORKERS", &c.MaxWorkers},
		{"MRORSYNC_PART_WORKERS", &c.PartWorkers},
		{"MRORSYNC_RETRY_TRIES", &c.Tuning.RetryTries},
		{"MRORSYNC_BACKOFF_MAX_SECONDS", &c.Tuning.BackoffMaxSeconds},
		{"MRORSYNC_BUSY_BACKOFF_MAX_SECONDS", &c.Tuning.BusyBackoffMaxSeconds},
		{"MRORSYNC_HTTP_TIMEOUT_SECONDS", &c.Tuning.HTTPTimeoutSeconds},
		{"MRORSYNC_DELETE_PROTECT_PERCENT", &c.Tuning.DeleteProtectPercent},
		{"MRORSYNC_DELETE_PROTECT_MIN", &c.Tuning.DeleteProtectMin},
		{"MRORSYNC_LOCK_STALE_HOURS", &c.Tuning.LockStaleHours},
		{"MRORSYNC_CACHE_GC_HOURS", &c.Tuning.CacheGCHours},
		{"MRORSYNC_VERIFY_PROGRESS_EVERY", &c.Tuning.VerifyProgressEvery},
	}
	for _, it := range ints {
		if err := setInt(it.key, it.dst); err != nil {
			return err
		}
	}

	if v, ok := os.LookupEnv("MRORSYNC_MAX_FILE_SIZE"); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fmt.Errorf("环境变量 MRORSYNC_MAX_FILE_SIZE=%q 不是整数", v)
		}
		c.MaxFileSize = n
	}
	return nil
}

func splitList(v string) []string {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
