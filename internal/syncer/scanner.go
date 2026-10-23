package syncer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mrorsync/internal/config"
)

// Entry is a local file candidate for upload.
type Entry struct {
	Rel     string // repo-relative path with "/" separators
	Size    int64
	Mtime   int64 // seconds, kept for old manifest records
	MtimeMS int64
}

// ScanResult is the outcome of walking the configured root.
type ScanResult struct {
	Entries []Entry
	Errors  int
	Samples []string
}

type excludeMatcher struct {
	exact map[string]bool
	wild  []*regexp.Regexp
	paths []string
}

func newExcludeMatcher(patterns, paths []string) *excludeMatcher {
	m := &excludeMatcher{exact: map[string]bool{}}
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if strings.Contains(p, "*") {
			parts := strings.Split(p, "*")
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			m.wild = append(m.wild, regexp.MustCompile("(?i)^"+strings.Join(parts, ".*")+"$"))
			continue
		}
		m.exact[strings.ToLower(p)] = true
	}
	for _, p := range paths {
		p = strings.Trim(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"), "/")
		if p == "" {
			continue
		}
		m.paths = append(m.paths, strings.ToLower(p))
	}
	return m
}

func (m *excludeMatcher) match(name string) bool {
	lower := strings.ToLower(name)
	if m.exact[lower] {
		return true
	}
	for _, re := range m.wild {
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}

// matchPath reports whether rel (a file or directory) is the excluded path or
// lives under it. Matching is case-insensitive on the '/' separated rel path.
func (m *excludeMatcher) matchPath(rel string) bool {
	lower := strings.ToLower(strings.ReplaceAll(rel, "\\", "/"))
	for _, p := range m.paths {
		if lower == p || strings.HasPrefix(lower, p+"/") {
			return true
		}
	}
	return false
}

// Scan walks cfg.Root, honouring excludes and the optional --only prefix.
// Symlinks/junctions are skipped so a link loop can never hang the backup.
func Scan(cfg *config.Config, only string, logf func(string, ...any)) (ScanResult, error) {
	matcher := newExcludeMatcher(cfg.Excludes, cfg.ExcludePaths)
	onlyKey := strings.ToLower(strings.Trim(strings.ReplaceAll(only, "\\", "/"), "/"))
	matchOnly := func(rel string) bool {
		if onlyKey == "" {
			return true
		}
		r := strings.ToLower(rel)
		return r == onlyKey || strings.HasPrefix(r, onlyKey+"/") || strings.HasPrefix(onlyKey, r+"/")
	}

	res := ScanResult{}
	stack := []string{""}
	for len(stack) > 0 {
		relDir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		absDir := cfg.Root
		if relDir != "" {
			absDir = filepath.Join(cfg.Root, filepath.FromSlash(relDir))
		}
		ents, err := os.ReadDir(absDir)
		if err != nil {
			res.Errors++
			if len(res.Samples) < 5 {
				res.Samples = append(res.Samples, relDir)
			}
			continue
		}
		for _, de := range ents {
			name := de.Name()
			if matcher.match(name) {
				continue
			}
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}
			if matcher.matchPath(rel) {
				continue
			}
			if !matchOnly(rel) {
				continue
			}
			if de.Type()&os.ModeSymlink != 0 {
				continue
			}
			if de.IsDir() {
				stack = append(stack, rel)
				continue
			}
			if !de.Type().IsRegular() {
				continue
			}
			info, err := de.Info()
			if err != nil {
				res.Errors++
				if len(res.Samples) < 5 {
					res.Samples = append(res.Samples, rel)
				}
				continue
			}
			if info.Size() > cfg.MaxFileSize {
				logf("[skip] 超过单文件上限 %s, 需拆包: %s", HumanBytes(cfg.MaxFileSize), rel)
				continue
			}
			res.Entries = append(res.Entries, Entry{
				Rel:     rel,
				Size:    info.Size(),
				Mtime:   info.ModTime().Unix(),
				MtimeMS: roundMillis(info.ModTime()),
			})
		}
	}
	return res, nil
}

// roundMillis matches JavaScript's Math.round(mtimeMs), which older manifests
// were written with: Go's UnixMilli truncates sub-millisecond fractions, which
// would differ by 1ms and cause needless re-hashing.
func roundMillis(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Nanosecond()%1_000_000 >= 500_000 {
		ms++
	}
	return ms
}
