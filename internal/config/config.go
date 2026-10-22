package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mrorsync/internal/fsutil"
)

const (
	DefaultMaxWorkers   = 2
	DefaultPartWorkers  = 2
	DefaultMaxFileSize  = int64(4 * 1024 * 1024 * 1024) // 4GB, 普通用户上限
	configFilePerm      = 0o600
	defaultRemoteParent = "/备份/D盘"
)

// Config mirrors config.json one-for-one, so configs written by older versions
// keep working without migration.
type Config struct {
	Root         string   `json:"root"`
	RemoteBase   string   `json:"remote_base"`
	AppID        string   `json:"app_id"`
	AppKey       string   `json:"app_key"`
	SecretKey    string   `json:"secret_key"`
	SignKey      string   `json:"sign_key"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	MaxWorkers   int      `json:"max_workers"`
	PartWorkers  int      `json:"part_workers"`
	MaxFileSize  int64    `json:"max_file_size"`
	StateDir     string   `json:"state_dir"`
	Excludes     []string `json:"excludes"`

	path string
	raw  map[string]json.RawMessage
}

// Default returns the values used when config.json omits a key.
func Default() *Config {
	return &Config{
		Root:        "D:\\",
		RemoteBase:  defaultRemoteParent,
		MaxWorkers:  DefaultMaxWorkers,
		PartWorkers: DefaultPartWorkers,
		MaxFileSize: DefaultMaxFileSize,
		Excludes: []string{
			"$RECYCLE.BIN", "System Volume Information", "node_modules", ".git", ".state",
		},
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
func DefaultPath() string { return filepath.Join(ExeDir(), "config.json") }

// Load reads config.json, preserving unknown fields so a save (token refresh)
// does not drop keys added by future versions.
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
	return c, nil
}

// Path returns the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// Normalize validates and canonicalises paths and limits.
func (c *Config) Normalize() error {
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
	c.MaxWorkers = clamp(c.MaxWorkers, DefaultMaxWorkers, 1, 16)
	c.PartWorkers = clamp(c.PartWorkers, DefaultPartWorkers, 1, 8)
	if c.MaxFileSize <= 0 {
		c.MaxFileSize = DefaultMaxFileSize
	}
	if c.Excludes == nil {
		c.Excludes = Default().Excludes
	}
	for i := range c.Excludes {
		c.Excludes[i] = strings.TrimSpace(c.Excludes[i])
	}
	if c.StateDir != "" {
		abs, err := filepath.Abs(c.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir 路径无效: %w", err)
		}
		c.StateDir = abs
	}
	return nil
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
