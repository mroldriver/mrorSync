package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNormalizeFillsDefaults(t *testing.T) {
	c := Default()
	c.Root = tempRoot(t)
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.ConfigVersion != CurrentVersion {
		t.Fatalf("config_version=%d", c.ConfigVersion)
	}
	if c.Tuning != defaultTuning() {
		t.Fatalf("tuning defaults not applied: %+v", c.Tuning)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", c.Warnings)
	}
}

func TestNormalizeWarnsOnClamp(t *testing.T) {
	c := Default()
	c.Root = tempRoot(t)
	c.MaxWorkers = 99
	c.Tuning.RetryTries = 99
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.MaxWorkers != 16 {
		t.Fatalf("max_workers=%d, want 16", c.MaxWorkers)
	}
	if c.Tuning.RetryTries != 10 {
		t.Fatalf("retry_tries=%d, want 10", c.Tuning.RetryTries)
	}
	if len(c.Warnings) != 2 {
		t.Fatalf("warnings=%v, want 2 entries", c.Warnings)
	}
}

func TestNormalizeRejectsFutureVersion(t *testing.T) {
	c := Default()
	c.Root = tempRoot(t)
	c.ConfigVersion = 2
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "config_version") {
		t.Fatalf("want config_version error, got %v", err)
	}
}

func TestLoadAppliesEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	root := tempRoot(t)
	raw := `{"root":` + quote(root) + `,"max_workers":2,"tuning":{"retry_tries":3}}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MRORSYNC_MAX_WORKERS", "7")
	t.Setenv("MRORSYNC_RETRY_TRIES", "4")
	t.Setenv("MRORSYNC_EXCLUDES", "*.log, *.tmp")

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxWorkers != 7 {
		t.Fatalf("max_workers=%d, want 7", c.MaxWorkers)
	}
	if c.Tuning.RetryTries != 4 {
		t.Fatalf("retry_tries=%d, want 4", c.Tuning.RetryTries)
	}
	if len(c.Excludes) != 2 || c.Excludes[0] != "*.log" || c.Excludes[1] != "*.tmp" {
		t.Fatalf("excludes=%v", c.Excludes)
	}
}

func TestLoadRejectsBadEnvInt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"root":"`+tempRoot(t)+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MRORSYNC_MAX_WORKERS", "many")
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("bad env int must fail")
	}
}

func TestEffectiveStateDirPrecedence(t *testing.T) {
	c := Default()
	c.StateDir = filepath.Join(t.TempDir(), "from-config")
	if got := c.EffectiveStateDir(); got != c.StateDir {
		t.Fatalf("config state dir not preferred: %s", got)
	}

	c.StateDir = ""
	t.Setenv("MRORSYNC_STATE_DIR", filepath.Join(t.TempDir(), "from-env"))
	if got := c.EffectiveStateDir(); got != os.Getenv("MRORSYNC_STATE_DIR") {
		t.Fatalf("env state dir not used: %s", got)
	}

	os.Unsetenv("MRORSYNC_STATE_DIR")
	t.Setenv("BAIDU_SYNC_STATE_DIR", filepath.Join(t.TempDir(), "legacy"))
	if got := c.EffectiveStateDir(); got != os.Getenv("BAIDU_SYNC_STATE_DIR") {
		t.Fatalf("legacy env state dir not used: %s", got)
	}
}

func TestMaskedHidesSecrets(t *testing.T) {
	c := Default()
	c.SecretKey = "secret"
	c.AccessToken = "access"
	c.RefreshToken = "refresh"
	c.SignKey = "sign"
	m := c.Masked()
	for name, v := range map[string]string{
		"secret_key":    m.SecretKey,
		"access_token":  m.AccessToken,
		"refresh_token": m.RefreshToken,
		"sign_key":      m.SignKey,
	} {
		if v != "******" {
			t.Fatalf("%s not masked: %q", name, v)
		}
	}
	if c.SecretKey != "secret" {
		t.Fatal("Masked mutated the original config")
	}
}

func TestDefaultPathHonorsEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom.json")
	t.Setenv("MRORSYNC_CONFIG", want)
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath=%s, want %s", got, want)
	}
}

func TestExcludePathsNormalize(t *testing.T) {
	root := tempRoot(t)
	inside := filepath.Join(root, "data", "cache")
	outside := filepath.Join(filepath.Dir(root), "elsewhere")

	c := Default()
	c.Root = root
	c.ExcludePaths = []string{
		"data\\cache", // backslashes -> slashes
		"/data/logs/", // surrounding slashes trimmed
		inside,        // absolute under root -> relative
		outside,       // outside root -> ignored with warning
		"DATA/CACHE",  // duplicate, case-insensitive
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := []string{"data/cache", "data/logs"}
	if len(c.ExcludePaths) != len(want) {
		t.Fatalf("exclude_paths=%v, want %v", c.ExcludePaths, want)
	}
	for i := range want {
		if c.ExcludePaths[i] != want[i] {
			t.Fatalf("exclude_paths[%d]=%q, want %q", i, c.ExcludePaths[i], want[i])
		}
	}
	if len(c.Warnings) != 1 {
		t.Fatalf("warnings=%v, want 1 (outside root)", c.Warnings)
	}
}

func TestLoadEnvExcludePaths(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	root := tempRoot(t)
	if err := os.WriteFile(cfgPath, []byte(`{"root":`+quote(root)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MRORSYNC_EXCLUDE_PATHS", "a/b;c\\d")
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ExcludePaths) != 2 || c.ExcludePaths[0] != "a/b" || c.ExcludePaths[1] != `c\d` {
		t.Fatalf("exclude_paths=%v", c.ExcludePaths)
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
